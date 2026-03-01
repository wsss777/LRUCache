package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/wsss777/LRUCache/logger"
	pb "github.com/wsss777/LRUCache/pb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type Client struct {
	addr    string
	svcName string
	etcdCli *clientv3.Client
	conn    *grpc.ClientConn
	grpcCli pb.WsCacheClient
}

var _ Peer = (*Client)(nil)

func NewClient(addr string, svcName string, etcdCli *clientv3.Client) (*Client, error) {
	var err error
	if etcdCli == nil {
		etcdCli, err = clientv3.New(clientv3.Config{
			Endpoints:   []string{"localhost:2379"},
			DialTimeout: 5 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create etcd client error: %v", err)
		}
	}
	//conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
	//	grpc.WithBlock(),
	//	grpc.WithTimeout(time.Second*10),
	//	grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	//)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second, // 每 30s 发一次 ping
			Timeout:             10 * time.Second,
			PermitWithoutStream: true, // 空闲时也发 ping
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial server : %v", err)
	}
	grpcClient := pb.NewWsCacheClient(conn)
	client := &Client{
		addr:    addr,
		svcName: svcName,
		etcdCli: etcdCli,
		conn:    conn,
		grpcCli: grpcClient,
	}
	return client, nil
}
func (c *Client) Get(group, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := c.grpcCli.Get(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get value from wsCache: %v", err)
	}

	return resp.GetValue(), nil
}
func (c *Client) Delete(group, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := c.grpcCli.Delete(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return false, fmt.Errorf("failed to delete value from wsCache: %v", err)
	}

	return resp.GetValue(), nil
}
func (c *Client) Set(ctx context.Context, group, key string, value []byte) error {
	resp, err := c.grpcCli.Set(ctx, &pb.Request{
		Group: group,
		Key:   key,
		Value: value,
	})
	if err != nil {
		return fmt.Errorf("failed to set value to wsCache: %v", err)
	}
	logger.L().Info("grpc set request resp",
		zap.Any("resp", resp))

	return nil
}
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
