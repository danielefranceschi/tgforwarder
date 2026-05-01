package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	botAPI, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return fmt.Errorf("create telegram bot: %w", err)
	}
	logger.Info("telegram bot authenticated", "bot_user", botAPI.Self.UserName)

	svc := &Service{
		Config: cfg,
		Forwarder: &Forwarder{
			HTTPClient: &http.Client{
				Timeout: 15 * time.Second,
			},
			MaxRetries:  3,
			BaseBackoff: 200 * time.Millisecond,
		},
		SendMessage: botAPI.Send,
		Logger:      logger,
		Now:         time.Now,
	}

	updateCfg := tgbotapi.NewUpdate(0)
	updateCfg.Timeout = 30
	updates := botAPI.GetUpdatesChan(updateCfg)
	defer botAPI.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}
			svc.processMessage(ctx, update.Message)
		}
	}
}
