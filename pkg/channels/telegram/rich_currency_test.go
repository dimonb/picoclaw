package telegram

import (
	"context"
	"strings"
	"testing"

	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/stretchr/testify/require"
)

const currencyRegression = "Куплено **28 YES по $0,09** на касание ETH уровня $2 200 или ниже до окончания недельного контракта — **$2,52 без комиссии**.\nМодель: 17–19%..."
const currencyRegressionEscaped = "Куплено **28 YES по \\$0,09** на касание ETH уровня \\$2 200 или ниже до окончания недельного контракта — **\\$2,52 без комиссии**.\nМодель: 17–19%..."

func TestParseContentRichCurrency(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"original Russian regression", currencyRegression, currencyRegressionEscaped},
		{"bold and italic boundaries", "**$10** __$30__", `**\$10** __\$30__`},
		{"angle autolink boundary", "<https://e.test/>$10", `<https://e.test/>\$10`},
		{"bracket prose", "[Pay $10] and $20", `[Pay \$10] and \$20`},
		{"unicode variables", "$2α$ and $α+β$ cost $10", `$2α$ and $α+β$ cost \$10`},
		{"unfinished unicode math", "$2α", "$2α"},
		{"multiplication math", "$2*3$ $2**3$ cost $10", `$2*3$ $2**3$ cost \$10`},
		{"unfinished multiplication", "$2**3 and $2*3", "$2**3 and $2*3"},
		{"italic currency", "*$10*, _$20_ and __$30__.", `*\$10*, _\$20_ and __\$30__.`},
		{"price lists", "$10, $20; $10-$20; $10/$20", `\$10, \$20; \$10-\$20; \$10/\$20`},
		{"latex prose", `$2 + \text{hello}$ costs $10`, `$2 + \text{hello}$ costs \$10`},
		{"unicode math", "$2 × 2$ and $2 ≤ 3$ cost $10", `$2 × 2$ and $2 ≤ 3$ cost \$10`},
		{"English pair", "Paid $10 and $20.", `Paid \$10 and \$20.`},
		{"bold", "**$2.52 total**, **$10**", `**\$2.52 total**, **\$10**`},
		{"mixed", "Use $x^2$ and $2+2$ for $10 today; $2$ stays math.", `Use $x^2$ and $2+2$ for \$10 today; $2$ stays math.`},
		{"numeric math", "$2$ $2+2$ $2 + 2 = 4$ $2x$ $2\\times 2$", "$2$ $2+2$ $2 + 2 = 4$ $2x$ $2\\times 2$"},
		{"display", "$$2 + 2\n= 4$$ costs $20.", "$$2 + 2\n= 4$$ costs \\$20."},
		{"indented code", "    $10\n    $20\n\nPay $30", "    $10\n    $20\n\nPay \\$30"},
		{"tab code", "\t$10\nPay $20", "\t$10\nPay \\$20"},
		{"quoted fence", "> ~~~\n> $10\n> ~~~\nPay $20", "> ~~~\n> $10\n> ~~~\nPay \\$20"},
		{"shortcut reference", "[$5]\n\n[$5]: https://e.test/$10\nPay $20", "[$5]\n\n[$5]: https://e.test/$10\nPay \\$20"},
		{"inline code", "`$10 and $20` costs $30", "`$10 and $20` costs \\$30"},
		{"multiline span", "``a ` $10\n$20`` costs $30", "``a ` $10\n$20`` costs \\$30"},
		{"fenced", "```go\n$10\n$20\n```\nPay $30", "```go\n$10\n$20\n```\nPay \\$30"},
		{"tilde fence", "~~~\n$10\n$20\n~~~\nPay $30", "~~~\n$10\n$20\n~~~\nPay \\$30"},
		{"long fence", "````\n```\n$10\n````\nPay $30", "````\n```\n$10\n````\nPay \\$30"},
		{"unfinished fence", "```\n$10\n$20", "```\n$10\n$20"},
		{"links", "[$10](https://e.test/$20) ![$5](https://e.test/i) pay $30", "[$10](https://e.test/$20) ![$5](https://e.test/i) pay \\$30"},
		{"nested link", "[a [$5]](https://e.test/a($10)) pay $20", "[a [$5]](https://e.test/a($10)) pay \\$20"},
		{"reference", "[$5][ref]\n[ref]: https://e.test/$10\nPay $20", "[$5][ref]\n[ref]: https://e.test/$10\nPay \\$20"},
		{"URL", "https://e.test/$10?q=$20 <https://e.test/$30> pay $40", "https://e.test/$10?q=$20 <https://e.test/$30> pay \\$40"},
		{"escaped", `\$10 and \$20; \\$30`, `\$10 and \$20; \\\$30`},
		{"grouping", "$2,200.50 and $2 200,50; $2 200", `\$2,200.50 and \$2 200,50; \$2 200`},
		{"unfinished math", "$2+2 and $2x", "$2+2 and $2x"},
		{"no currency", "# Title\n| a | b |\n|---|---|\n**yes**", "# Title\n| a | b |\n|---|---|\n**yes**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseContent(tt.in, telegramFormatRich)
			require.Equal(t, tt.want, got)
			require.Equal(t, got, parseContent(got, telegramFormatRich), "must be idempotent")
		})
	}
}

func TestRichCurrencyDeliveryPaths(t *testing.T) {
	for _, path := range []string{"send", "edit", "draft", "finalize"} {
		t.Run(path, func(t *testing.T) {
			caller := &stubCaller{callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
				if strings.Contains(url, "Draft") {
					return &ta.Response{Ok: true, Result: []byte("true")}, nil
				}
				return successResponseWithMessageID(t, 7), nil
			}}
			ch := newRichTestChannel(t, caller)
			switch path {
			case "send":
				_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "12345", Content: currencyRegression})
				require.NoError(t, err)
			case "edit":
				require.NoError(t, ch.EditMessage(context.Background(), "12345", "7", currencyRegression))
			default:
				ch.tgCfg.Streaming = config.StreamingConfig{Enabled: true}
				streamer, err := ch.BeginStream(context.Background(), "12345")
				require.NoError(t, err)
				if path == "draft" {
					require.NoError(t, streamer.Update(context.Background(), currencyRegression))
				} else {
					require.NoError(t, streamer.Finalize(context.Background(), currencyRegression))
				}
			}
			found := false
			for _, call := range caller.calls {
				body := decodeJSONBody(t, call.Data)
				if rich, ok := body["rich_message"].(map[string]any); ok {
					require.Equal(t, currencyRegressionEscaped, rich["markdown"])
					found = true
				}
			}
			require.True(t, found)
		})
	}
}
