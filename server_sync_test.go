package wscache

import (
	"context"
	"testing"
	"time"

	"github.com/wsss777/LRUCache/cache"
	"github.com/wsss777/LRUCache/cluster"
	pb "github.com/wsss777/LRUCache/pb"
	"google.golang.org/grpc/metadata"
)

const peerRequestMetadataKey = "x-wscache-peer-request"

type spyPeerPicker struct {
	called chan struct{}
}

func (s *spyPeerPicker) PickPeer(key string) (cluster.Peer, bool, bool) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil, false, false
}

func (s *spyPeerPicker) Close() error {
	return nil
}

func TestServerSetDoesNotResyncPeerRequests(t *testing.T) {
	picker := &spyPeerPicker{called: make(chan struct{}, 1)}
	group := cache.NewGroup("peer-set-no-resync", 1024, cache.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			return []byte("value"), nil
		}),
		cache.WithPeers(picker),
	)
	defer group.Close()

	srv := &Server{}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(peerRequestMetadataKey, "1"))

	if _, err := srv.Set(ctx, &pb.Request{
		Group: "peer-set-no-resync",
		Key:   "k",
		Value: []byte("v"),
	}); err != nil {
		t.Fatalf("server set failed: %v", err)
	}

	select {
	case <-picker.called:
		t.Fatal("peer request should not trigger another sync")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestServerDeleteDoesNotResyncPeerRequests(t *testing.T) {
	picker := &spyPeerPicker{called: make(chan struct{}, 1)}
	group := cache.NewGroup("peer-delete-no-resync", 1024, cache.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			return []byte("value"), nil
		}),
		cache.WithPeers(picker),
	)
	defer group.Close()

	if err := group.Set(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("seed value: %v", err)
	}
	select {
	case <-picker.called:
	case <-time.After(150 * time.Millisecond):
	}

	srv := &Server{}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(peerRequestMetadataKey, "1"))

	if _, err := srv.Delete(ctx, &pb.Request{
		Group: "peer-delete-no-resync",
		Key:   "k",
	}); err != nil {
		t.Fatalf("server delete failed: %v", err)
	}

	select {
	case <-picker.called:
		t.Fatal("peer delete should not trigger another sync")
	case <-time.After(150 * time.Millisecond):
	}
}
