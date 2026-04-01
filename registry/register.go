package registry

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/wsss777/LRUCache/logger"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// Config 定义 etcd 客户端配置。
type Config struct {
	Endpoints   []string
	DialTimeout time.Duration
}

// DefaultConfig 提供默认配置。
var DefaultConfig = &Config{
	Endpoints:   []string{"localhost:2379"},
	DialTimeout: 5 * time.Second,
}

// NormalizeAddress 将 ":8001" 这样的监听地址转换成服务发现中发布的地址格式。
func NormalizeAddress(address string) (string, error) {
	if address == "" {
		return "", fmt.Errorf("address is empty")
	}
	if address[0] != ':' {
		return address, nil
	}

	localIP, err := getLocalIP()
	if err != nil {
		return "", fmt.Errorf("failed to get local ip: %v", err)
	}
	return fmt.Sprintf("%s%s", localIP, address), nil
}

// Register 将服务注册到 etcd，并持续续租。
func Register(svcName string, address string, stopCh <-chan error) error {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   DefaultConfig.Endpoints,
		DialTimeout: DefaultConfig.DialTimeout,
	})
	if err != nil {
		return fmt.Errorf("failed to create etcd client: %v", err)
	}

	address, err = NormalizeAddress(address)
	if err != nil {
		cli.Close()
		return err
	}

	lease, err := cli.Grant(context.Background(), 10)
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to create etcd lease: %v", err)
	}

	key := fmt.Sprintf("/services/%s/%s", svcName, address)
	_, err = cli.Put(context.Background(), key, address, clientv3.WithLease(lease.ID))
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to put key-value to etcd: %v", err)
	}

	keepAliveCh, err := cli.KeepAlive(context.Background(), lease.ID)
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to keep lease alive to etcd: %v", err)
	}

	go func() {
		defer cli.Close()
		for {
			select {
			case <-stopCh:
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				cli.Revoke(ctx, lease.ID)
				cancel()
				return
			case resp, ok := <-keepAliveCh:
				if !ok {
					logger.L().Warn("keep alive channel closed")
					return
				}
				logger.L().Debug("successfully renewed lease",
					zap.Int("resp.ID", int(resp.ID)))
			}
		}
	}()

	logger.L().Info("Service register success",
		zap.String("svcName", svcName),
		zap.String("address", address))
	return nil
}

func getLocalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}

			if ip[0] == 169 && ip[1] == 254 {
				continue
			}
			if ip[0] == 172 && ip[1] == 20 {
				continue
			}
			if ip[0] == 192 && ip[1] == 168 {
				continue
			}

			return ip.String(), nil
		}
	}

	return "127.0.0.1", nil
}
