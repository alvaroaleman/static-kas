package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/util/sets"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/alvaroaleman/static-kas/pkg/handler"
)

const Port string = "8080"

type options struct {
	baseDir string
	kubeCfg string
}

func main() {

	o := options{}
	flag.StringVar(&o.baseDir, "base-dir", "", "The basedir of the cluster dump")
	flag.StringVar(&o.kubeCfg, "kubeconfig", "", "Path to a kubeconfig file. If set, --base-dir will be searched for multiple dumps and a kubeconfig with a context for each of them will be generated")
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

	if o.kubeCfg == "" {
		router, err := handler.New(l, o.baseDir, Port)
		if err != nil {
			l.Fatal("failed to construct server", zap.Error(err))
		}
		if err := http.ListenAndServe(fmt.Sprintf(":%s", Port), router); err != nil {
			l.Error("server ended", zap.Error(err))
		}

	} else {
		baseDirs := sets.NewString(o.baseDir)

		if err := filepath.Walk(o.baseDir, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return nil
			}
			if filepath.Base(path) != "namespaces" {
				return nil
			}
			l.Info("Found dump", zap.String("path", filepath.Dir(path)))
			baseDirs.Insert(filepath.Dir(path))
			return nil
		}); err != nil {
			l.Fatal("failed to walk to find additional dumps", zap.Error(err))
		}

		baseDirPortMapping := make(map[string]int, len(baseDirs))
		for _, baseDir := range baseDirs.List() {
			baseDir := baseDir
			l := l.With(zap.String("baseDir", baseDir))
			listener, err := net.Listen("tcp", "")
			if err != nil {
				l.Fatal("failed to construct listener", zap.Error(err))
			}
			baseDirPortMapping[baseDir] = listener.Addr().(*net.TCPAddr).Port
			go func() {
				router, err := handler.New(l, baseDir, strconv.Itoa(baseDirPortMapping[baseDir]))
				if err != nil {
					l.Fatal("failed to construct handler", zap.Error(err))
				}
				server := &http.Server{Handler: router}
				if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
					l.Fatal("server ended unexpectedly", zap.Error(err))
				}
			}()
		}
		kubeCfg := clientcmdapi.Config{
			Kind:           "Config",
			APIVersion:     "v1",
			Clusters:       map[string]*clientcmdapi.Cluster{},
			Contexts:       map[string]*clientcmdapi.Context{},
			CurrentContext: o.baseDir,
		}
		for baseDir, port := range baseDirPortMapping {
			kubeCfg.Clusters[baseDir] = &clientcmdapi.Cluster{Server: "http://127.0.0.1:" + strconv.Itoa(port)}
			kubeCfg.Contexts[baseDir] = &clientcmdapi.Context{Cluster: baseDir}
		}
		serialized, err := clientcmd.Write(kubeCfg)
		if err != nil {
			l.Fatal("Failed to serialize kubeconfig", zap.Error(err))
		}
		if err := os.WriteFile(o.kubeCfg, serialized, 0644); err != nil {
			l.Fatal("Failed to write kubeconfig", zap.Error(err))
		}
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}
