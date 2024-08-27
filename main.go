package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PaulSonOfLars/gotgbot/parsemode"
	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/filters/message"
	"github.com/joho/godotenv"
	log "github.com/sirupsen/logrus"
	"github.com/zmb3/spotify/v2"
	auth "github.com/zmb3/spotify/v2/auth"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	EnvProduction  = "PROD"
	EnvDevelopment = "DEV"
)

var spotifyClient *spotify.Client

func getEnvOrFatal(key string) string {
	value := os.Getenv(key)
	if value == "" {
		exit(fmt.Sprintf("Environment variable not set: %s", key))
	}
	return value
}

func exit(msg string) {
	_, _ = fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func main() {
	var timeout int64
	var logLevel string

	flag.StringVar(&logLevel, "log-level", "debug", "Set the log level (debug, info, warn, error, fatal, panic)")
	flag.Int64Var(&timeout, "timeout", 0, "Set the timeout value in seconds")
	flag.Parse()

	if timeout < 0 {
		exit("Timeout value must be greater than 0")
	}
	level, err := log.ParseLevel(logLevel)
	if err != nil {
		exit(fmt.Sprintf("Unknown log level: %s", logLevel))
	}
	log.SetLevel(level)

	err = godotenv.Load()
	if err != nil {
		log.WithError(err).Fatal("Error loading .env file")
	}
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = EnvDevelopment
	}
	if environment != EnvProduction {
		log.SetFormatter(&log.TextFormatter{
			ForceColors:      true,
			DisableQuote:     true,
			QuoteEmptyFields: true,
			FullTimestamp:    true,
		})
	}
	log.Debug("Environment: ", environment)

	telegramToken := getEnvOrFatal("TELEGRAM_TOKEN")
	spotifyClientID := getEnvOrFatal("SPOTIFY_CLIENT_ID")
	spotifyClientSecret := getEnvOrFatal("SPOTIFY_CLIENT_SECRET")

	bot, err := gotgbot.NewBot(telegramToken, nil)
	if err != nil {
		log.WithError(err).Fatal("Failed to create new bot")
	}

	// Set up a Spotify API client
	config := &clientcredentials.Config{
		ClientID:     spotifyClientID,
		ClientSecret: spotifyClientSecret,
		TokenURL:     auth.TokenURL,
	}

	token, err := config.Token(context.Background())
	if err != nil {
		log.WithError(err).Fatal("Error during Spotify token creation")
	}

	httpClient := auth.New().Client(context.Background(), token)
	spotifyClient = spotify.New(httpClient)

	dispatcher := ext.NewDispatcher(&ext.DispatcherOpts{
		// If a handler returns an error, log it and continue going.
		Error: func(b *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
			log.WithError(err).Error("An error occurred while handling update")
			return ext.DispatcherActionEndGroups
		},
	})
	dispatcher.AddHandlerToGroup(&HandleAnything{}, -1)

	dispatcher.AddHandler(handlers.NewMessage(message.Audio, handleAudioMessage))
	dispatcher.AddHandler(handlers.NewMessage(message.All, handleUnknownMessage))

	updater := ext.NewUpdater(
		dispatcher,
		&ext.UpdaterOpts{
			UnhandledErrFunc: func(err error) {
				log.WithError(err).Error("Updater error")
			},
		},
	)
	err = updater.StartPolling(
		bot,
		&ext.PollingOpts{
			DropPendingUpdates: false,
			GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
				Timeout:        timeout,
				AllowedUpdates: []string{},
				RequestOpts: &gotgbot.RequestOpts{
					Timeout: time.Second * time.Duration(timeout+10),
				},
			},
		},
	)
	if err != nil {
		log.WithError(err).Fatal("Failed to start polling")
	}

	log.Info("Bot started: https://t.me/", bot.User.Username)

	if environment == EnvProduction {
		// We don't care about graceful shutdown in development
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-signals
			log.Info("Received shutdown signal, stopping bot...")
			s := time.Now().UnixMilli()
			errStop := updater.Stop()
			f := time.Now().UnixMilli()
			sec := float64(f-s) / 1000
			log.Debugf("Time took to stop %.3f", sec)
			if errStop != nil {
				log.WithError(errStop).Error("Unable to stop bot")
				return
			}
		}()
	}

	updater.Idle()
}

type HandleAnything struct {
	ext.Handler
}

func (h *HandleAnything) CheckUpdate(_ *gotgbot.Bot, _ *ext.Context) bool {
	return true
}

func (h *HandleAnything) HandleUpdate(_ *gotgbot.Bot, ctx *ext.Context) error {
	if log.IsLevelEnabled(log.DebugLevel) {
		// explicit log level check to avoid useless json manipulation
		raw, err := json.Marshal(&ctx.Update)
		if err != nil {
			return fmt.Errorf("failed to marshal update: %w", err)
		}
		log.WithFields(log.Fields{
			"data": string(raw),
		}).Debug("Handling update")
	}

	return ext.ContinueGroups
}

func (h *HandleAnything) Name() string {
	return "anything"
}

func handleAudioMessage(bot *gotgbot.Bot, ctx *ext.Context) (err error) {
	msg := ctx.EffectiveMessage
	title := strings.TrimSpace(msg.Audio.Title)
	author := strings.TrimSpace(msg.Audio.Performer)
	query := strings.TrimSpace(fmt.Sprintf("%s %s", title, author))

	if query == "" {
		query = msg.Audio.FileName
		query = strings.TrimSuffix(query, ".mp3")
		query = strings.TrimSpace(query)
	}
	if query == "" {
		_, errSendMsg := msg.Reply(bot, "Audio metadata or filename is missing.", nil)
		checkSendMsgErr(errSendMsg)
		return fmt.Errorf("audio metadata or filename is missing")
	}

	results, err := searchSpotify(query)
	if err != nil {
		_, errSendMsg := msg.Reply(bot, "Failed to search Spotify.", nil)
		checkSendMsgErr(errSendMsg)
		return err
	}

	searchBtn := gotgbot.InlineKeyboardButton{
		Text: "Search",
		Url:  buildSpotifyUserSearchURL(query),
	}
	total := results.Tracks.Total
	if total == 0 {
		text := fmt.Sprintf("No results found on Spotify by query `%s`", query)
		opts := &gotgbot.SendMessageOpts{
			ReplyMarkup: &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{searchBtn}},
			},
		}
		_, err = msg.Reply(bot, text, opts)
		return
	}
	track := results.Tracks.Tracks[0]
	text, err := buildResultText(&track)
	if err != nil {
		return err
	}
	_, errSendMsg := msg.Reply(bot, text,
		&gotgbot.SendMessageOpts{
			ParseMode: parsemode.Html,
			LinkPreviewOptions: &gotgbot.LinkPreviewOptions{
				PreferSmallMedia: true,
				ShowAboveText:    true,
			},
			ReplyMarkup: &gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: [][]gotgbot.InlineKeyboardButton{{searchBtn}},
			},
		},
	)
	return errSendMsg
}

func checkSendMsgErr(err error) {
	if err == nil {
		return
	}
	log.WithError(err).Error("Failed to send message")
}

func handleUnknownMessage(bot *gotgbot.Bot, ctx *ext.Context) error {
	_, errSendMsg := ctx.EffectiveMessage.Reply(bot, "Send me an audio file to search on Spotify.", nil)
	return errSendMsg
}

type trackData struct {
	Name, Artists, URL string
}

func searchSpotify(query string) (*spotify.SearchResult, error) {
	return spotifyClient.Search(context.Background(), query, spotify.SearchTypeTrack)
}

// buildSpotifyUserSearchURL constructs a Spotify search URL for the user with the given query.
func buildSpotifyUserSearchURL(query string) string {
	baseURL := "https://open.spotify.com/search"
	return fmt.Sprintf("%s/%s", baseURL, url.PathEscape(query))
}

func buildResultText(track *spotify.FullTrack) (text string, err error) {
	artists := make([]string, len(track.Artists))
	for i, artist := range track.Artists {
		artists[i] = artist.Name
	}

	buf := bytes.Buffer{}
	tpl, err := template.New("track").Parse(`<a href="{{.URL}}">{{.Name}}</a>
by <b>{{.Artists}}</b>`)
	if err != nil {
		return
	}
	td := trackData{
		Name:    track.Name,
		Artists: strings.Join(artists, ", "),
		URL:     track.ExternalURLs["spotify"],
	}
	if err = tpl.Execute(&buf, td); err != nil {
		return
	}
	return buf.String(), nil
}
