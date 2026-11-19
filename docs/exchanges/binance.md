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

## `venuecheck` ile ölçülecekler

- Saat farkı ve gidiş-dönüş süresi.
- USDT paritelerinin sayısı ve 24 saatlik hacme göre ilk 20 (coin evreni kararı D-4 için).
- BTCUSDT ve ETHUSDT için tick, step, minimum notional, OCO, STP modları, en iyi fiyat farkı.
- Rate limit başlıklarının adları ve değerleri.
- Anahtar verilirse: hesap bayrakları, ücret oranları ve anahtar yetkileri (uyarılarla).

## Bilinmeyenler

- Türkiye'den binance.com erişiminin süreklilik durumu (SPK erişim engeli riski, D-17'de kabul edildi).
- Delist ve bakım duyurularının hangi kanaldan izleneceği.
