package ws

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkDecodeAggTrade(b *testing.B) {
	msg := []byte(fmt.Sprintf(aggTradeMsg, 12345))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := Decode(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeDepth decodes a 100ms diff with 20 bid and 20 ask levels.
func BenchmarkDecodeDepth(b *testing.B) {
	var lv []string
	for i := range 20 {
		lv = append(lv, fmt.Sprintf(`["%d.01","0.12345"]`, 90000+i))
	}
	levels := strings.Join(lv, ",")
	msg := []byte(`{"stream":"btcusdt@depth@100ms","data":{"e":"depthUpdate","E":1700000000000,"s":"BTCUSDT","U":101,"u":120,"b":[` +
		levels + `],"a":[` + levels + `]}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := Decode(msg); err != nil {
			b.Fatal(err)
		}
	}
}
