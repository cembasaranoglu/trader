# ADR-005: Birincil borsa binance.com (USDT)

- Durum: Kabul edildi (2026-11-19); ADR-001'deki borsa varsayımını değiştirir
- Kaynak: Plan dokümanı, karar D-17

## Bağlam

Plan başta Binance TR (TRY pariteleri) üzerine kuruldu (D-3). Kullanıcının Binance TR hesabı yok, binance.com hesabı var. binance.com anahtarları Binance TR API'sinde çalışmaz.

## Karar

- Birincil borsa binance.com, kote para birimi USDT.
- `internal/exchange/binance` adaptörü eklendi. Config yalnız `venue.primary: binance` ve `quote_currency: USDT` kabul eder.
- `internal/exchange/binancetr` yalnız `venuecheck` tanı aracında kalır. WebSocket, emir ve mutabakat katmanları onun için yazılmaz; Binance TR'ye dönüş yeni bir ADR gerektirir.
- İmza için HMAC ve Ed25519 desteklenir. Canlı işlem anahtarı için Ed25519 ve VPS IP kısıtlaması önerilir.

## Kabul edilen riskler

- binance.com SPK yetkili değil (Kanun 7518 Md. 99/A). Yaptırım platforma yönelik; erişim engeli veya çekim sorunu kullanıcıyı etkileyebilir.
- Kote para birimi bir stablecoin (USDT). R-21 (`risk.fx_shock_pct`) USDT/USD depeg korumasına dönüşür.

## Sonuçlar

- P&L ve limitler USDT cinsinden tutulur. Vergi defteri için her dolumda USD/TRY kuru kaydedilir.
- binance.com Demo Mode (`demo-api.binance.com`, 2026-01-29'dan beri) emir yolunun entegrasyon testi için kullanılır; paper simülatörünün yerine geçmez (fiyatlar canlıya "benzer", eşit değil). Bkz. `docs/plan/01-waves.md`.
- Anahtar yetkileri API'den okunabildiği için, "çekim yetkisi kapalı mı" kontrolü elle değil programla yapılır.
