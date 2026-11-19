# ADR-005: Birincil borsa binance.com (USDT)

- Durum: Kabul edildi (2026-11-19); ADR-001'deki borsa varsayımını değiştirir
- Kaynak: Plan dokümanı, karar D-17

## Bağlam

Plan başta Binance TR (TRY pariteleri) üzerine kuruldu (D-3). Kullanıcının Binance TR hesabı yok, binance.com hesabı var. binance.com anahtarları Binance TR API'sinde çalışmaz.

## Karar

- Birincil borsa binance.com, kote para birimi USDT.
- `internal/exchange/binance` adaptörü eklendi. `internal/exchange/binancetr` takılabilir seçenek olarak kalır; config ikisini de kabul eder ve borsa ile kote para biriminin eşleşmesini zorlar (binance → USDT, binance_tr → TRY).
- İmza için HMAC ve Ed25519 desteklenir. Canlı işlem anahtarı için Ed25519 ve VPS IP kısıtlaması önerilir.

## Kabul edilen riskler

- binance.com SPK yetkili değil (Kanun 7518 Md. 99/A). Yaptırım platforma yönelik; erişim engeli veya çekim sorunu kullanıcıyı etkileyebilir.
- Kote para birimi bir stablecoin (USDT). R-21 (`risk.fx_shock_pct`) USDT/USD depeg korumasına dönüşür.

## Sonuçlar

- P&L ve limitler USDT cinsinden tutulur. Vergi defteri için her dolumda USD/TRY kuru kaydedilir.
- binance.com Demo Mode, W2'deki paper simülatörüne alternatif olarak değerlendirilecek.
- Anahtar yetkileri API'den okunabildiği için, "çekim yetkisi kapalı mı" kontrolü elle değil programla yapılır.
