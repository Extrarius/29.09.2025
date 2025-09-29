package main

import (
	"log"
	"net/http"
	"time"

	httpapi "github.com/Extrarius/29.09.2025/internal/http"

	"github.com/Extrarius/29.09.2025/internal/app"
)

func main() {
	conf := app.Config{
		Port:            env("PORT", "8080"),
		DataDir:         env("DATA_DIR", "./data"),
		DownloadDir:     env("DOWNLOAD_DIR", "./downloads"),
		Workers:         envInt("WORKERS", 4),
		HostConcurrency: envInt("HOST_CONCURRENCY", 2),
		ClientTimeout:   envDuration("CLIENT_TIMEOUT", 60*time.Second),
		Retries:         envInt("RETRIES", 3),
		ShutdownWait:    envDuration("SHUTDOWN_WAIT", 20*time.Second),
	}
	application, err := app.New(conf)
	if err != nil {
		log.Fatalf("app init: %v", err)
	}
	defer application.Close()

	router := httpapi.NewRouter(application) //+++
	if err := application.Serve(router); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
