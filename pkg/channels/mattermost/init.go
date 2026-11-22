package mattermost

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterSafeFactory(config.ChannelMattermost,
		func(bc *config.Channel, c *config.MattermostSettings, b *bus.MessageBus) (channels.Channel, error) {
			return NewMattermostChannel(bc, c, b)
		})
}
