# ADR-004: Katı konfigürasyon, kodda varsayılan yok

- Durum: Kabul edildi (2026-11-19)
- Kaynak: Plan dokümanı, karar D-16

## Bağlam

Risk limitleri kişisel kararlardır. Yanlış yazılmış bir anahtarın sessizce yok sayılması ya da eksik bir alanın sıfır değeri alması, risk motorunu fark edilmeden devre dışı bırakabilir.

## Karar

- YAML çözümlemesi `KnownFields(true)` ile yapılır; bilinmeyen anahtar hatadır.
- Her alan zorunludur. Kodda varsayılan değer yoktur; önerilen değerler `configs/example.yaml` içindedir.
- Eksik alan `ErrMissing`, aralık dışı değer ya da alanlar arası kural ihlali `ErrInvalid`, çözümleme sorunu `ErrDecode` ile döner. Tüm sorunlar tek seferde, alan yoluyla birlikte raporlanır.
- Doğrulama tek yerde yapılır (`internal/config`). `configs/schema.json` editör desteği içindir; testler şema ile örnek dosyanın aynı anahtarları tanımladığını kontrol eder.
- Süreler birimleriyle yazılır (`60m`); birimsiz sayı reddedilir.
- `venue.secondary_enabled: true`, BtcTurk adaptörü gelene kadar (W9) reddedilir.

## Sonuçlar

- `*config.Config` taşıyan her kod, değerlerin eksiksiz ve geçerli olduğuna güvenebilir.
- Yeni bir alan eklemek beş dosyayı birlikte değiştirir: `raw.go`, `config.go`, `load.go`, `configs/example.yaml`, `configs/schema.json`. Alan `example.yaml` ile `raw.go` ya da `schema.json` arasında eşleşmezse testler başarısız olur. `load.go` içindeki doğrulamanın yazılmış olması testlerle otomatik yakalanmaz; kod incelemesinde kontrol edilir.
