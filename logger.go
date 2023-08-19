package main

import (
	"fmt"
	"os"

	"go.elara.ws/logger"
	"go.elara.ws/logger/log"
)

func init() {
	l := logger.NewPretty(os.Stderr)

	if os.Getenv("LEMMY_REPLY_BOT_DEBUG") == "1" {
		l.Level = logger.LogLevelDebug
	}

	log.Logger = l
}
