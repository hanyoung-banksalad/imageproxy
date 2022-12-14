package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanyoung-banksalad/imageproxy/client"
	"github.com/hanyoung-banksalad/imageproxy/config"
	"github.com/hanyoung-banksalad/imageproxy/server"
	"github.com/hanyoung-banksalad/imageproxy/server/redis"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/banksalad/go-banksalad"
)

func main() {
	ctx := context.Background()

	logrus.SetFormatter(&logrus.JSONFormatter{})
	log := logrus.StandardLogger()

	setting := config.NewSetting()

	statsdCli := banksalad.NewStatsdClient(banksalad.StatsdOptions{
		StatsdAddr: fmt.Sprintf("localhost:%s", setting.StatsdUDPPort),
		Prefix:     fmt.Sprintf("%s.%s", setting.Namespace, setting.ServiceName),
		Logger:     log,
	})
	defer func() {
		if err := statsdCli.Close(); err != nil {
			log.WithError(err).Error("failed to close statsd client")
		}
	}()

	logrus.AddHook(banksalad.NewLogLevelStatHook(statsdCli))
	logrus.AddHook(banksalad.NewLogStacktraceHook())

	closeTracer, err := banksalad.InitializeTracer(banksalad.TracerOptions{
		ServiceName: setting.ServiceName,
		Namespace:   setting.Namespace,
		Env:         setting.Env,
	})
	if err != nil {
		log.WithError(err).Fatal("failed to initialize jaeger client")
	}
	defer closeTracer()

	authCli := client.GetAuthClient(setting.AuthGRPCEndpoint, setting.ServiceName)

	redisCli, err := redis.NewClient(
		setting.RedisHost,
		setting.RedisPoolSize,
		setting.RedisMinIdleConns,
		setting.RedisExpiresInMinutes,
		//setting.RedisEncryptionKey,
		//setting.RedisEncryptionNonce,
	)
	if err != nil {
		log.WithError(err).Fatal("failed to initialize redis client")
	}
	defer func() {
		if err := redisCli.Close(); err != nil {
			log.WithError(err).Error("failed to close redis client")
		}
	}()

	cfg := config.NewConfig(
		setting,
		statsdCli,
		authCli,
		redisCli,
	)

	grpcServer, err := server.NewGRPCServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	httpServer, err := server.NewHTTPServer(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.Setting().GRPCServerPort)
		if err != nil {
			log.WithError(err).Fatal("net.Listen")
		}

		log.WithField("port", cfg.Setting().GRPCServerPort).Info("starting imageproxy gRPC server")
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Fatal(err)
		}
	}()

	go func() {
		log.WithField("port", cfg.Setting().HTTPServerPort).Info("starting imageproxy HTTP server")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	<-quit

	time.Sleep(time.Duration(cfg.Setting().GracefulShutdownTimeoutMs) * time.Millisecond)

	log.Info("stopping imageproxy HTTP server")
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}

	log.Info("stopping imageproxy gRPC server")
	grpcServer.GracefulStop()
}
