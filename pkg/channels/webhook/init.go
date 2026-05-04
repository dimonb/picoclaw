package webhook

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterSafeFactory(config.ChannelWebhook,
		func(bc *config.Channel, c *config.WebhookSettings, b *bus.MessageBus) (channels.Channel, error) {
			return NewWebhookChannel(bc, c, b)
		})
}
