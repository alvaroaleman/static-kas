package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/alvaroaleman/static-kas/pkg/handler"
)

const Port string = "8080"

type options struct {
	baseDir string
}

func main() {

	o := options{}
	flag.StringVar(&o.baseDir, "base-dir", "", "The basedir of the cluster dump")
	flag.Parse()

	lCfg := zap.NewProductionConfig()
	lCfg.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	l, err := lCfg.Build()
	if err != nil {
		fmt.Printf("failed to construct logger: %v\n", err)
		os.Exit(1)
	}
	defer l.Sync()

	if o.baseDir == "" {
		l.Fatal("--base-dir is mandatory")
	}

	router, err := handler.New(l, o.baseDir, Port)
	if err != nil {
		l.Fatal("failed to construct server", zap.Error(err))
	}

	if err := http.ListenAndServe(fmt.Sprintf(":%s", Port), router); err != nil {
		l.Error("server ended", zap.Error(err))
	}
}
