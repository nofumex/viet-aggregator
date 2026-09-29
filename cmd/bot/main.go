package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/nofumex/telegram-aggregator/internal/collections"
	"github.com/nofumex/telegram-aggregator/internal/config"
	"github.com/nofumex/telegram-aggregator/internal/exchange"
	"github.com/nofumex/telegram-aggregator/internal/llm"
	"github.com/nofumex/telegram-aggregator/internal/mtproto"
	"github.com/nofumex/telegram-aggregator/internal/ranking"
	"github.com/nofumex/telegram-aggregator/internal/storage"
	"github.com/nofumex/telegram-aggregator/internal/syncer"
	tg "github.com/nofumex/telegram-aggregator/internal/telegram"
	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
	"github.com/nofumex/telegram-aggregator/internal/workers"
	"github.com/nofumex/telegram-aggregator/migrations"
)

func main() {
	_ = godotenv.Load()
	cfg, e := config.Load()
	if e != nil {
		fatal(e)
	}
	if cfg.TelegramToken == "" {
		fatal(errors.New("TELEGRAM_BOT_TOKEN is required"))
	}
	if cfg.ProfileLLM.BaseURL == "" || cfg.ProfileLLM.APIKey == "" {
		fatal(errors.New("BASE_URL and FREE_LLM_API_KEY are required for channel profile creation"))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	uiMax, bgMax := poolPartition(cfg.DBMaxConns, cfg.DBBackgroundMaxConns)
	store, e := storage.Open(ctx, cfg.DatabaseURL, uiMax, min(cfg.DBMinConns, uiMax))
	if e != nil {
		fatal(e)
	}
	defer store.Close()
	background := store
	if bgMax > 0 {
		background, e = storage.Open(ctx, cfg.DatabaseURL, bgMax, 0)
		if e != nil {
			fatal(e)
		}
		defer background.Close()
	}
	if e = migrations.Up(ctx, store.DB); e != nil {
		fatal(e)
	}
	profileLLM := llm.New(llm.Config{Provider: "compatible", BaseURL: cfg.ProfileLLM.BaseURL, APIKey: cfg.ProfileLLM.APIKey, Model: cfg.ProfileLLM.Model, Timeout: cfg.ProfileLLM.Timeout, Concurrency: 1})
	rank := ranking.NewWithConfig(background.RankingConfig(ctx))
	account := mtproto.New(background)
	syncService := syncer.New(background, telegramfeed.New(), profileLLM, rank, log, cfg.WorkerConcurrency, account)
	collectionService := collections.NewWithRanking(background, rank, log)
	api := tg.NewClient(cfg.TelegramToken)
	rates := exchange.NewCBR()
	bot := tg.NewBot(api, store, syncService, collectionService, rates, cfg.AdminIDs, log, cfg.DefaultPoll, account)
	syncService.SetProfileReadyHandler(bot.NotifyProfileReady)
	server := healthServer(cfg.HTTPAddr, store)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "error", err)
			stop()
		}
	}()
	go syncService.Run(ctx)
	go account.Run(ctx)
	go workers.RunReranking(ctx, background, rank, cfg, log)
	go collectionService.Run(ctx, cfg.CollectionRefreshInterval, cfg.CollectionRefreshTimeout)
	go rates.RunRefresh(ctx, 15*time.Minute)
	go func() {
		if err := bot.Run(ctx); err != nil {
			log.Error("telegram bot stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	log.Info("shutdown complete")
}

func poolPartition(total, requested int) (ui, background int) {
	if total < 2 {
		return 1, 0
	}
	background = requested
	if background < 1 {
		background = 1
	}
	if background > total-1 {
		background = total - 1
	}
	return total - background, background
}
func healthServer(addr string, s *storage.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if e := s.DB.Ping(ctx); e != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ready\n"))
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, e); os.Exit(1) }
