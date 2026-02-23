package logger

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func L() *zap.Logger {
	return logger
}

func S() *zap.SugaredLogger {
	return sugar
}

var (
	once   sync.Once
	logger *zap.Logger = zap.NewNop() // 默认 nop，避免 nil panic
	sugar  *zap.SugaredLogger
)

func init() {
	once.Do(func() {
		// 读取环境变量
		env := os.Getenv("ENV")
		logLevelStr := os.Getenv("LOG_LEVEL")

		//确定是否生产模式
		isProd := false
		switch strings.ToLower(strings.TrimSpace(env)) {
		case "production", "prod", "p":
			isProd = true
		case "development", "dev", "d":
			isProd = false
		default:
			// 默认开发模式
			isProd = false
		}

		//创建基础配置
		var cfg zap.Config
		if isProd {
			cfg = zap.NewProductionConfig()
			cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
			cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
		} else {
			cfg = zap.NewDevelopmentConfig()
			cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
			cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
			cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
		}

		//如果设置了 LOG_LEVEL，则尝试覆盖
		if logLevelStr != "" {
			trimmed := strings.TrimSpace(strings.ToLower(logLevelStr))
			var targetLevel zapcore.Level
			err := targetLevel.UnmarshalText([]byte(trimmed))
			if err == nil {
				cfg.Level = zap.NewAtomicLevelAt(targetLevel)
			} else {
				//记录无效配置
				fmt.Fprintf(os.Stderr, "无效的 LOG_LEVEL 值: %q，已忽略，使用默认级别\n", logLevelStr)
			}
		}

		//构建 logger
		l, err := cfg.Build(
			zap.AddCaller(),
			zap.AddStacktrace(zap.ErrorLevel),
			zap.AddCallerSkip(1),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zap 初始化失败: %v，使用 nop logger\n", err)
			l = zap.NewNop()
		}

		logger = l
		sugar = l.Sugar()
	})
}
