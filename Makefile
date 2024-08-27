export CGO_ENABLED=0

export PACKAGE=main.go
export BUILD=./build/find_audio_on_spotify_bot
export DEPLOY_TO=~/apps/telegram-bot-audio-find-spotify/find_audio_on_spotify_bot

run:
	go run ${PACKAGE}
.PHONY: run

logfmt:
	go run ${PACKAGE} | logfmt -l DEBUG
.PHONY: logfmt

build:
	go build -o ${BUILD} ${PACKAGE}
.PHONY: build

upload:
	rsync -P -h ${BUILD} www-data@iho.su:${DEPLOY_TO}
.PHONY: upload

deps:
	go mod tidy
	go mod vendor
.PHONY: deps

ready: deps build upload
.PHONY: ready
