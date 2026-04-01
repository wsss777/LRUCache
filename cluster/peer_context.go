package cluster

import (
	"context"

	"google.golang.org/grpc/metadata"
)

type peerRequestContextKey struct{}
type localOnlyContextKey struct{}

const peerRequestMetadataKey = "x-wscache-peer-request"
const localOnlyMetadataKey = "x-wscache-local-only"

// WithPeerRequest 给上下文打上内部节点请求标记。
func WithPeerRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, peerRequestContextKey{}, true)
	return metadata.AppendToOutgoingContext(ctx, peerRequestMetadataKey, "1")
}

func WithLocalOnly(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, localOnlyContextKey{}, true)
	return metadata.AppendToOutgoingContext(ctx, localOnlyMetadataKey, "1")
}

// IsPeerRequest 判断请求是否来自其他缓存节点。
func IsPeerRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if peerRequest, ok := ctx.Value(peerRequestContextKey{}).(bool); ok && peerRequest {
		return true
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, value := range md.Get(peerRequestMetadataKey) {
		if value == "1" {
			return true
		}
	}
	return false
}

func IsLocalOnlyRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if localOnly, ok := ctx.Value(localOnlyContextKey{}).(bool); ok && localOnly {
		return true
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, value := range md.Get(localOnlyMetadataKey) {
		if value == "1" {
			return true
		}
	}
	return false
}
