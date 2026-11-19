# trader

LLM destekli, kripto spot piyasasında (binance.com, USDT pariteleri) çalışan bir işlem sistemi. LLM ajanları yalnızca şemalı öneri üretir. Emirlere deterministik Go çekirdeğindeki risk motoru karar verir.

> **Durum:** Wave W1. Konfigürasyon doğrulayıcı ve binance.com ile Binance TR için salt okuma istemcileri (saat, sembol kuralları, piyasa verisi, hesap bayrakları, API anahtarı yetkileri) var. Emir gönderme ve LLM katmanı **yok**. Sistem para ile işlem yapmaz.
>
> Bu yazılım yatırım tavsiyesi değildir. Kâr garantisi yoktur.

Tasarım ve dalga planı: [Plan dokümanı](https://claude.ai/artifact/7Rgd1jjuXpecY2bMzy1NAH). Mimari kararlar `docs/adr/` altında.

## Gereksinimler

- Go 1.24.7 (`go.mod`)

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
go run ./cmd/venuecheck -venue binance_tr -out venuecheck-report.json
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

binance.com'da anahtarın yetkileri (`/sapi/v1/account/apiRestrictions`) da okunur. Çekim yetkisi açıksa, IP kısıtlaması olmadan işlem yetkisi varsa ya da gereksiz başka bir yetki açıksa rapor `warnings` altında uyarır. Kimlik dosyası ve Ed25519 anahtar dosyası grup veya diğer kullanıcılar tarafından okunabiliyorsa komut çalışmaz. Şifreli (parolalı) Ed25519 anahtarları desteklenmez.

Ayrıntılar: [docs/exchanges/binance.md](docs/exchanges/binance.md), [docs/exchanges/binance-tr.md](docs/exchanges/binance-tr.md).

## Konfigürasyon

- `configs/example.yaml`: onaylanan değerlerle (D-16) örnek. Sır içermez.
- `configs/schema.json`: editör desteği için JSON Schema.
- `internal/config`: asıl doğrulayıcı. Bilinmeyen anahtarları reddeder, her alanı zorunlu tutar, kodda varsayılan değer yoktur, alanlar arası kuralları (ör. günlük kayıp ≤ haftalık kayıp ≤ max drawdown) kontrol eder.

API anahtarları hiçbir zaman config dosyasına, koda ya da loglara yazılmaz. Bkz. [SECURITY.md](SECURITY.md).

## Geliştirme

```sh
gofmt -l .
go vet ./...
go test -race ./...
go build ./...
```

CI aynı adımları `.github/workflows/ci.yml` içinde çalıştırır.
