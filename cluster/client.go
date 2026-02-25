package cluster

import (
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

type Client struct {
	addr    string
	svcName string
	etcdCli *clientv3.Client
	conn    *grpc.ClientConn
	grpcCli pb.CacheClient
}
