# trader

LLM destekli, kripto spot piyasasında (binance.com, USDT pariteleri) çalışan bir işlem sistemi. LLM ajanları yalnızca şemalı öneri üretir. Emirlere deterministik Go çekirdeğindeki risk motoru karar verir.

> **Durum:** W1a tamam; W1b (WebSocket piyasa verisi) ve W1c (depolama, ön kontrol) kodlandı, canlı doğrulama bekliyor. Konfigürasyon doğrulayıcı, binance.com salt okuma REST istemcisi (saat senkronu, sembol kuralları, piyasa verisi, hesap bayrakları, API anahtarı yetkileri), `venuecheck` tanı aracı ve WebSocket piyasa verisi (`traderd observe`) var. Hash zincirli olay günlüğü ve başlangıç ön kontrolü (`traderd preflight`) var. Emir gönderme ve LLM katmanı **yok**. Sistem para ile işlem yapmaz. Sıradaki işler: [docs/plan/01-waves.md](docs/plan/01-waves.md).
>
> Bu yazılım yatırım tavsiyesi değildir. Kâr garantisi yoktur.

Tasarım ve dalga planı: [Plan dokümanı](https://claude.ai/artifact/7Rgd1jjuXpecY2bMzy1NAH). Mimari kararlar `docs/adr/` altında.

## Gereksinimler

- Go 1.26 (`go.mod`; `toolchain go1.26.11`, eski bir Go kuruluysa `GOTOOLCHAIN=auto` ile otomatik indirilir)

## Kullanım

```sh
go build ./cmd/traderd
./traderd -config configs/example.yaml
```

Çıkış kodları: `0` konfigürasyon geçerli, `1` geçersiz (her sorun ayrı satırda listelenir), `2` kullanım hatası.

## Borsa doğrulaması (`venuecheck`)

`venuecheck`, dokümandan doğrulanamayan borsa bilgilerini canlı API'de ölçer ve paylaşılabilir bir JSON rapor üretir. Raporda sır ve bakiye tutarı yoktur. İstekler arasında 250 ms bekler.

Yalnızca herkese açık veri (anahtar gerekmez):

```sh
go run ./cmd/venuecheck -out venuecheck-report.json            # binance.com (varsayılan)
go run ./cmd/venuecheck -venue binance_tr -out venuecheck-report.json   # yalnız tanı amaçlı
```

Hesap bayrakları ve anahtar yetkileri için **yalnız okuma** yetkili bir anahtarla:

```sh
mkdir -p ~/.config/trader
# Dosyayı bir editörle oluştur; anahtarı terminal geçmişine yazma.
# HMAC anahtarı:    {"api_key": "...", "secret_key": "..."}
# Ed25519 anahtarı: {"api_key": "...", "ed25519_private_key_file": "/home/<sen>/.config/trader/ed25519.pem"}
chmod 600 ~/.config/trader/binance.json
go run ./cmd/venuecheck -credentials ~/.config/trader/binance.json -out venuecheck-report.json
```

binance.com'da anahtarın yetkileri (`/sapi/v1/account/apiRestrictions`) da okunur. Çekim yetkisi açıksa, IP kısıtlaması olmadan işlem yetkisi varsa ya da gereksiz başka bir yetki açıksa rapor `warnings` altında uyarır. İmzalı isteklerden önce yerel saat borsa saatine göre ölçülür ve istek zaman damgasına uygulanır.

Kimlik dosyası kuralları: Linux/macOS'ta kimlik dosyası ve Ed25519 anahtar dosyası sembolik bağlantı olamaz, çalıştıran kullanıcıya ait olmalı ve yalnız sahibi okuyabilmeli (`chmod 600`); aksi halde komut çalışmaz. Windows'ta dosya erişim listesi (ACL) kontrol edilemez; komut bunu uyarı olarak yazar. Göreli `ed25519_private_key_file` yolu kimlik dosyasının bulunduğu klasöre göre çözülür. Şifreli (parolalı) Ed25519 anahtarları desteklenmez.

Ayrıntılar: [docs/exchanges/binance.md](docs/exchanges/binance.md), [docs/exchanges/binance-tr.md](docs/exchanges/binance-tr.md).

## Piyasa verisi gözlemi (`traderd observe`)

WebSocket piyasa verisini (kapanmış mumlar, en iyi fiyat, işlemler, yerel emir defteri) belirli bir süre çalıştırır ve dakikada bir JSON satırı yazar: defter yeniden kurmaları, boşluklar, REST ile doldurulan mumlar, stale süresi, bağlantı devirleri, iki hostun ağırlık başlıkları. Anahtar gerekmez.

```sh
go run ./cmd/traderd observe -config configs/example.yaml -symbols BTCUSDT,ETHUSDT,SOLUSDT -duration 24h -out observe.jsonl
```

Son satır `"type":"final"` özetidir; aynı özet ekrana da yazılır. Ctrl-C ile erken durdurulursa özet `"interrupted":true` ile yazılır. `-out` verildiğinde her satır diske `fsync` ile yazılır (dosyanın klasörü de bir kez); süreç ya da makine düşerse o ana kadarki satırlar kalır. Standart çıktıya yazarken bu garanti yoktur.

Kısa doğrulama (1 saat, bağlantı devri her 20 dakikada; 24 saat beklemeden devri sınar):

```sh
go run ./cmd/traderd observe -config configs/example.yaml -symbols BTCUSDT,ETHUSDT,SOLUSDT -duration 1h -rotate-after 20m -out observe-1h.jsonl
```

`-rotate-after` 1 dakika ile 23 sa 50 dk arasında olmalıdır; verilmezse 23 sa 50 dk'dır (Binance bağlantıyı 24 saatte kapatır). 24 saatlik koşu bunun yerine sunucunun kendi davranışını (gerçek 24 saat sınırı yaklaşırken devir, `serverShutdown`, günlük mum kapanışı) ve saatler içinde ortaya çıkan büyümeyi ölçer; üretimde VPS'te sürekli çalışan süreçle aynı koşuldur.

Raporda uzun koşu için ek alanlar:

| Alan | Anlamı | Beklenen |
| --- | --- | --- |
| `runtime.goroutines`, `runtime.heap_alloc_bytes` | Süreç büyümesi | Saatler boyunca sabit bir bant; sürekli artış sızıntıdır |
| `ws.queue_high_water` | İşleyiciyi bekleyen en çok mesaj (kapasite 1024) | Küçük; 1024'e yaklaşması işleyicinin yetişemediğini gösterir |
| `ws.max_delivery_lag_ms` | Okuma ile işleme arasındaki en uzun gecikme | Milisaniyeler |
| `ws.rotations`, `ws.rotation_failures`, `ws.handovers` | Bağlantı devirleri. Devirde yeni bağlantının mesajları kısa bir süre (1 sn) bekletilir, eski bağlantı okunmaya devam eder; böylece eski bağlantıda yolda olan mesajlar kaybolmaz | Devir başına `book_gaps` ve `trade_id_gaps` artmamalı |
| `ws.retired_dropped` | Devirden sonra eski bağlantıdan kuyrukta kalan, yenisinin zaten taşıdığı mesajlar | Küçük sayılar normaldir |
| `clock.jumps` | Duvar saati sıçramaları: NTP düzeltmesi, elle değişiklik; Linux ve macOS'ta uyku (bu sistemlerde monoton saat uykuda durur). Windows'ta uyku burada görünmez | 0; Linux/macOS dizüstünde uyku olduysa > 0 |

Veri tazeliği (R-9) ve bağlantının sessiz kalma süresi iki saatle ölçülür, büyük olan geçerlidir: monoton saat duvar saatinin geri atlamasından, duvar saati uykudan etkilenmez. Böylece uyanıştan sonra saatlerce eski veri taze görünmez; ileri doğru bir saat sıçraması en fazla gereksiz bir yeniden bağlantıya ya da bir sonraki olaya kadar "stale" işaretine yol açar.

Dizüstünde koşarken uyku moduna geçmeyi engelleyin: macOS `caffeinate -i go run ...`, Linux `systemd-inhibit --what=idle:sleep go run ...`, Windows'ta Güç ayarlarından uyku "Hiçbir zaman". Uyku sırasında bağlantı düşer. Uyanınca bekçi bunu bir sonraki kontrolünde (en geç 15 sn) görür ve yeniden bağlanır; o ana kadar veri "stale" sayılır. Rapor bunu `ws.disconnects` (Linux/macOS'ta ayrıca `clock.jumps`) olarak gösterir.

## Başlangıç ön kontrolü ve depolama

`traderd preflight` işlem süreci başlamadan önce şunları borsadan doğrular: saat farkı (R-20), API anahtarında çekim yetkisinin kapalı olması ve ek yetki bulunmaması (I-7), işlem yetkisi varsa IP kısıtının açık olması, seçilen STP modunun her sembolde izinli olması (R-22) ve olay günlüğünün hash zincirinin sağlam olması. Bir kontrol bile başarısızsa çıkış kodu 1'dir.

```sh
go run ./cmd/traderd preflight -config configs/example.yaml -credentials ~/.config/trader/binance.json -symbols BTCUSDT,ETHUSDT
go run ./cmd/traderd store verify -config configs/example.yaml
go run ./cmd/traderd store backup -config configs/example.yaml
```

Ayrıntılar ve geri yükleme adımları: [docs/runbook/storage.md](docs/runbook/storage.md).

## Konfigürasyon

- `configs/example.yaml`: onaylanan değerlerle (D-16) örnek. Sır içermez.
- `configs/schema.json`: editör desteği için JSON Schema.
- `internal/config`: asıl doğrulayıcı. Bilinmeyen anahtarları reddeder, her alanı zorunlu tutar, kodda varsayılan değer yoktur, alanlar arası kuralları (ör. günlük kayıp ≤ haftalık kayıp ≤ max drawdown) kontrol eder.

API anahtarları hiçbir zaman config dosyasına, koda ya da loglara yazılmaz. Bkz. [SECURITY.md](SECURITY.md).

## Geliştirme

```sh
gofmt -l .
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go test -race ./...
go build ./...
python3 scripts/gen_schema.py configs/example.yaml > configs/schema.json   # config alanı değişince
```

CI aynı adımları `.github/workflows/ci.yml` içinde çalıştırır.
