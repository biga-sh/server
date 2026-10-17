package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Channel is the Redis pub/sub channel every instance shares.
const Channel = "biga:events"

// RedisBridge relays events through Redis (or Valkey) pub/sub.
type RedisBridge struct {
	client *redis.Client
	log    *slog.Logger
}

// NewRedisBridge returns a bridge over client.
func NewRedisBridge(client *redis.Client, log *slog.Logger) *RedisBridge {
	return &RedisBridge{client: client, log: log}
}

// Publish sends msg to every instance.
func (b *RedisBridge) Publish(ctx context.Context, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return b.client.Publish(ctx, Channel, msg).Err()
}

// Run delivers messages from the channel until ctx is done. go-redis
// resubscribes on its own after a dropped connection.
func (b *RedisBridge) Run(ctx context.Context, deliver func([]byte)) {
	sub := b.client.Subscribe(ctx, Channel)
	defer sub.Close()
	ch := sub.Channel(redis.WithChannelSize(1024))
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			deliver([]byte(m.Payload))
		}
	}
}
