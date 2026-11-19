# Binance TR: doğrulanmış ve doğrulanacak bilgiler

Kaynak: [Binance TR Open API](https://www.binance.tr/apidocs), 2026-11-19'da okundu, ve aynı gün kullanıcının makinesinde çalıştırılan `venuecheck` ölçümü.

## Dokümana göre (adaptörde uygulandı)

| Konu | Bilgi |
| --- | --- |
| REST host | `https://www.binance.tr`, yollar `/open/v1/...` |
| Piyasa verisi | Symbol type 1: `https://api.binance.me` (`/api/v3/depth`, `/api/v1/klines`), sembolde `_` kaldırılır. Diğer tipler: `https://cloudme-tr.2meta.app` (`/api/v1/...`) |
| Cevap zarfı | `{"code":0,"msg":"success","data":...,"timestamp":...}`; `code != 0` hata. Bazı emir uçları `msg` yerine `message` kullanır |
| İmza | HMAC-SHA256, `X-MBX-APIKEY` başlığı, `timestamp` zorunlu, `recvWindow` varsayılan 5000 ms (en fazla 60000) |
| Rate limit | IP başına ağırlık; `429` sonra `418` ban (2 dk–3 gün), `Retry-After` başlığı; `X-MBX-USED-WEIGHT-*` başlıkları. Sayısal limitler yayımlanmamış |
| `5XX` | İşlemin sonucu bilinmiyor; emir uçlarında sorgu ile mutabakat gerekir |
| Hesap | `GET /open/v1/account/spot` (imzalı): `canTrade`, `canWithdraw`, `canDeposit` hesap düzeyindedir, API anahtarının yetkilerini göstermez |
| Ücret | `fiatMakerCommission` / `fiatTakerCommission` kesir olarak (örnek `0.001500` = %0,15) |

## Canlı ölçüm (venuecheck, 2026-11-19 05:56 UTC, kimlik bilgisi olmadan)

Tek bir ölçümdür; spread ve saat farkı ana göre değişir.

| Konu | Ölçülen | Dokümanla karşılaştırma |
| --- | --- | --- |
| Saat farkı | Medyan +60 ms (sunucu yerelden ileride), en yüksek gidiş-dönüş 471 ms | `ops.max_clock_skew_ms: 500` ile uyumlu |
| Sembol sayısı | 309 sembolün tamamı TRY paritesi, tamamı spot işleme açık, tamamı symbol type 1 | "Next" host'u (tip 2/3) şu an kullanılmıyor |
| Piyasa verisi formatı | Depth ve klines **ham** (zarfsız) Binance formatında döndü | Doküman zarf gösteriyordu; adaptör ikisini de kabul ettiği için sorun çıkmadı |
| BTC_TRY kuralları | Tick 1 TRY, step 0,00001, min miktar 0,00001, **min notional 10 TRY**, OCO açık | Market bayrağı `false` okundu, ancak o sürüm NOTIONAL filtresinde `applyMinToMarket` alanını okumuyordu; bu bayrak **doğrulanmamış** sayılır ve yeniden ölçülmeli |
| ETH_TRY kuralları | Tick 1 TRY, step 0,0001, min miktar 0,0001, **min notional 10 TRY**, OCO açık | — |
| Emir tipleri | LIMIT, LIMIT_MAKER, MARKET, STOP_LOSS, STOP_LOSS_LIMIT, TAKE_PROFIT, TAKE_PROFIT_LIMIT | Doküman örneğinde olmayan STOP_LOSS ve TAKE_PROFIT de var |
| STP | Varsayılan EXPIRE_MAKER; izinli: EXPIRE_TAKER, EXPIRE_MAKER, EXPIRE_BOTH, DECREMENT, TRANSFER | Config'teki `EXPIRE_TAKER` destekleniyor |
| En iyi fiyat farkı (spread) | BTC_TRY 0,003 bps, ETH_TRY 0,11 bps | Yalnız en iyi seviye; derinlik ölçülmedi |
| Rate limit başlığı | `x-mbx-used-weight` ve `x-mbx-used-weight-1m`, yaklaşık 11 istekten sonra 14 | Sayısal üst limit hâlâ bilinmiyor |

Sonuçlar:

- Minimum emir tutarı 10 TRY olduğu için 40 USD'lik emir tavanı (D-16) borsa minimumuna takılmaz.
- İlk raporda son mumun yaşı negatif çıktı, çünkü en yeni mum henüz kapanmamıştı. Ölçüm artık en son **kapanmış** mumun yaşını raporluyor (`last_closed_kline_age_sec`).

## API'nin gösteremediği (elle kontrol)

- API anahtarının yetkileri (okuma / işlem / çekim).
- IP kısıtlaması desteği.
- Delist ve bakım duyurularının yayımlandığı kanal.
- `clientOrderId` tekrar davranışı ve testnet varlığı (dokümanda bulunamadı).
- Hesap bayrakları ve TRY ücret oranları: yalnız okuma yetkili anahtarla `venuecheck -credentials` çalıştırılınca ölçülecek.
