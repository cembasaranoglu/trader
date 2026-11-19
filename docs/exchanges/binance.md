# binance.com: doğrulanmış ve doğrulanacak bilgiler

Karar D-17 (2026-11-19) ile birincil borsa binance.com oldu. Bu dosya, adaptörün dayandığı dokümantasyon bilgilerini ve henüz canlı ölçülmeyenleri ayırır. Geliştirme ortamı binance.com'a ağ erişimine sahip değil; canlı ölçüm `venuecheck` ile kullanıcının makinesinde yapılır.

## Dokümana göre (adaptörde uygulandı)

| Konu | Bilgi | Kaynak |
| --- | --- | --- |
| Hostlar | İmzalı uçlar `https://api.binance.com`; herkese açık piyasa verisi `https://data-api.binance.vision` | [Spot REST API](https://developers.binance.com/zh-CN/docs/products/spot/rest-api) |
| Anahtar tipleri | HMAC, RSA ve Ed25519. HMAC imzası hex, Ed25519 ve RSA imzası base64; imza sorguda URL-encode edilir | [Request security](https://developers.binance.com/docs/binance-spot-api-docs/websocket-api/request-security) |
| Zaman | `timestamp` milisaniye ya da mikrosaniye; `recvWindow` en fazla 60000 ms, 5000 veya altı öneriliyor | [Spot REST API](https://developers.binance.com/zh-CN/docs/products/spot/rest-api) |
| Anahtar yetkileri | `GET /sapi/v1/account/apiRestrictions`: `ipRestrict`, `enableReading`, `enableSpotAndMarginTrading`, `enableWithdrawals` ve diğerleri. Çekim yetkisi ancak IP kısıtlamasıyla açılabilir | [API key permission](https://developers.binance.com/legacy-docs/wallet/account/api-key-permission) |
| Rate limit | `429` sonra `418`, `Retry-After`; `X-MBX-USED-WEIGHT-*` başlıkları | Araştırma raporu A |
| Ücret | %0,10 maker / %0,10 taker, BNB ile %0,075 | Araştırma raporu A |

Kullanıcının binance.com arayüzünde gördüğü kural: "Spot & Margin & Stock Trading" yetkisi ancak IP erişim kısıtlaması uygulandıktan sonra açılabiliyor.

## Canlı ölçüm (venuecheck, 2026-11-19 07:20 UTC, kimlik bilgisi olmadan)

Tek bir ölçümdür; hacim sıralaması, spread ve gecikme ana göre değişir.

| Konu | Ölçülen |
| --- | --- |
| Saat farkı | Medyan +54 ms; en yüksek gidiş-dönüş 1419 ms (tek örnek; diğerleri daha düşük) |
| Semboller | 3750 sembol; 782 USDT paritesi, bunların 525'i spot işleme açık |
| BTCUSDT | Tick 0,01, step 0,00001, **min notional 5 USDT** (market emirlerine de uygulanıyor), OCO açık |
| ETHUSDT | Tick 0,01, step 0,0001, **min notional 5 USDT** (market emirlerine de uygulanıyor), OCO açık |
| Emir tipleri | LIMIT, LIMIT_MAKER, MARKET, STOP_LOSS, STOP_LOSS_LIMIT, TAKE_PROFIT, TAKE_PROFIT_LIMIT |
| STP | Varsayılan EXPIRE_MAKER; EXPIRE_TAKER izinli |
| En iyi fiyat farkı | BTCUSDT 0,0016 bps, ETHUSDT 0,055 bps (yalnız en iyi seviye) |
| Rate limit | Ölçüm sonunda `x-mbx-used-weight-1m` = 119 (exchangeInfo ve 24h ticker en ağır istekler) |

24 saatlik USDT hacim sıralamasının ilk 20'si kripto olmayan varlıklar da içeriyor:

| Grup | Semboller (ilk 20 içinde) |
| --- | --- |
| Kripto | BTC, ETH, SOL, ZEC, XRP, NEAR, BNB, MMT, HYPE, TRX, DOGE, ENA |
| Stablecoin | USDC, USD1, RLUSD |
| Tokenize hisse/ETF (assumption: "…B" ekli semboller) | NVDAB, SOXLB, CRCLB |
| Altın token | XAUT, PAXG |

Sonuçlar:

- 40 USD emir tavanı (D-16), 5 USDT minimum emir tutarının çok üstünde.
- Coin evreni (D-4) yalnız hacimle seçilemez: D-1 "kripto" kararı gereği stablecoin, tokenize hisse/ETF ve emtia tokenları dışlanmalı. Bu sembolleri API'nin ayırt edip etmediği **unknown**; W2'de evren seçici açık bir dışlama listesiyle başlar.

## Bilinmeyenler

- Türkiye'den binance.com erişiminin süreklilik durumu (SPK erişim engeli riski, D-17'de kabul edildi).
- Delist ve bakım duyurularının hangi kanaldan izleneceği.
