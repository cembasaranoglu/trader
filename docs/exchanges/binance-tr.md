# Binance TR: doğrulanmış ve doğrulanacak bilgiler

Kaynak: [Binance TR Open API](https://www.binance.tr/apidocs), 2026-11-19'da okundu. Geliştirme ortamı Binance TR'ye ağ erişimine sahip olmadığı için aşağıdaki "dokümana göre" bilgiler canlı API'de henüz ölçülmedi. `venuecheck` komutu bunları ölçer.

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

## `venuecheck` ile ölçülecekler

- Saat farkı (sunucu − yerel) ve gidiş-dönüş süresi.
- TRY paritelerinin sayısı ve symbol type dağılımı (dokümanda tip 1 "Main", 2 "Next"; host notunda ise tip 3 geçiyor; tutarsız).
- İzlenen pariteler için tick, step, minimum miktar, minimum notional, OCO ve STP modları.
- Piyasa verisi hostlarının zarf mı ham format mı döndürdüğü (adaptör ikisini de kabul eder ve hangisini aldığını raporlar).
- Rate limit başlıklarının gerçek adları ve değerleri.
- Kimlik bilgisi verilirse: hesap bayrakları ve TRY ücret oranları. Bakiye tutarları rapora yazılmaz.

## API'nin gösteremediği (elle kontrol)

- API anahtarının yetkileri (okuma / işlem / çekim).
- IP kısıtlaması desteği.
- Delist ve bakım duyurularının yayımlandığı kanal.
- `clientOrderId` tekrar davranışı ve testnet varlığı (dokümanda bulunamadı).
