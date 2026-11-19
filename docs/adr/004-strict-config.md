# ADR-004: Katı konfigürasyon, kodda varsayılan yok

- Durum: Kabul edildi (2026-11-19)
- Kaynak: Plan dokümanı, karar D-16

## Bağlam

Risk limitleri kişisel kararlardır. Yanlış yazılmış bir anahtarın sessizce yok sayılması ya da eksik bir alanın sıfır değeri alması, risk motorunu fark edilmeden devre dışı bırakabilir.

## Karar

- YAML çözümlemesi `KnownFields(true)` ile yapılır; bilinmeyen anahtar hatadır.
- Her alan zorunludur. Kodda varsayılan değer yoktur; önerilen değerler `configs/example.yaml` içindedir.
- Eksik alan `ErrMissing`, aralık dışı değer ya da alanlar arası kural ihlali `ErrInvalid`, çözümleme sorunu (bozuk YAML, yanlış tipte değer, bilinmeyen anahtar) `ErrDecode` ile döner.
- Çözümleme hatası varsa alan doğrulaması çalışmaz ve yalnız o hata döner. Belge çözümlenebiliyorsa tüm eksik/geçersiz alanlar tek seferde, alan yoluyla birlikte raporlanır.
- Sayılar sonlu olmalıdır (`.inf`, `.nan` reddedilir). Tam sayı alanlarına ondalıklı değer yazılamaz (`10.9` kesilmez, reddedilir). Boolean yalnız `true`/`false`. Her alanın taşmayı önleyen bir üst sınırı vardır (ör. `max_data_age_ms ≤ 60000`, `max_clock_skew_ms ≤ 60000`, pencere 1m–24h).
- Çözümleme hata mesajlarında ters tırnak içindeki değerler maskelenir; yanlış yere yazılmış bir sır hata çıktısına düşmez.
- Doğrulama tek yerde yapılır (`internal/config`). `configs/schema.json` editör desteği içindir ve `scripts/gen_schema.py` ile örnek dosyadan üretilir; CI şemanın güncel olduğunu ve şema ile örneğin aynı anahtarları tanımladığını kontrol eder.
- Süreler birimleriyle yazılır (`60m`); birimsiz sayı reddedilir.
- Uygulanmamış bir yeteneği açan değerler reddedilir: `venue.secondary_enabled: true` (BtcTurk adaptörü yok) ve `venue.primary: binance_tr` (ticaret katmanı yalnız binance.com için yazılıyor, ADR-005) ve `mode: live_small` / `live_full` (canlı işlem W8'de, G1 kapısından sonra açılır).

## Sonuçlar

- `*config.Config` taşıyan her kod, değerlerin eksiksiz ve geçerli olduğuna güvenebilir.
- Yeni bir alan eklemek beş dosyayı birlikte değiştirir: `raw.go`, `config.go`, `load.go`, `configs/example.yaml`, `configs/schema.json`. Alan `example.yaml` ile `raw.go` ya da `schema.json` arasında eşleşmezse testler başarısız olur. `load.go` içindeki doğrulamanın yazılmış olması testlerle otomatik yakalanmaz; kod incelemesinde kontrol edilir.
