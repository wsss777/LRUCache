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
)

type Client struct {
	addr    string
	svcName string
	etcdCli *clientv3.Client
	conn    *grpc.ClientConn
	grpcCli pb.WsCacheClient
}

var _ Peer = (*Client)(nil)
var _ MigrationPeer = (*Client)(nil)

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

	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
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

func (c *Client) GetLocalEntry(group, key string) ([]byte, time.Time, error) {
	ctx, cancel := context.WithTimeout(WithLocalOnly(context.Background()), 3*time.Second)
	defer cancel()

	resp, err := c.grpcCli.Get(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to get local value from wsCache: %v", err)
	}

	expireAt := time.Time{}
	if resp.GetExpireAtUnixNano() > 0 {
		expireAt = time.Unix(0, resp.GetExpireAtUnixNano())
	}
	return resp.GetValue(), expireAt, nil
}

func (c *Client) Delete(group, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(WithPeerRequest(context.Background()), 3*time.Second)
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
	return c.SetWithExpireAt(ctx, group, key, value, time.Time{})
}

func (c *Client) SetWithExpireAt(ctx context.Context, group, key string, value []byte, expireAt time.Time) error {
	ctx = WithPeerRequest(ctx)

	req := &pb.Request{
		Group: group,
		Key:   key,
		Value: value,
	}
	if !expireAt.IsZero() {
		req.ExpireAtUnixNano = expireAt.UnixNano()
	}

	resp, err := c.grpcCli.Set(ctx, req)
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
