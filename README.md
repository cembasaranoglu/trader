# trader

LLM destekli, kripto spot piyasasında (Binance TR, TRY pariteleri) çalışan bir işlem sistemi. LLM ajanları yalnızca şemalı öneri üretir. Emirlere deterministik Go çekirdeğindeki risk motoru karar verir.

> **Durum:** Wave W1a. Konfigürasyon doğrulayıcı ve Binance TR için salt okuma istemcisi (saat, sembol kuralları, piyasa verisi, hesap bayrakları) var. Emir gönderme ve LLM katmanı **yok**. Sistem para ile işlem yapmaz.
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

## Binance TR doğrulaması (`venuecheck`)

`venuecheck`, dokümandan doğrulanamayan Binance TR bilgilerini canlı API'de ölçer ve paylaşılabilir bir JSON rapor üretir. Raporda sır ve bakiye tutarı yoktur. Varsayılan ayarlarla en fazla 11 istek yapar, istekler arasında 250 ms bekler.

Yalnızca herkese açık veri:

```sh
go run ./cmd/venuecheck -symbols BTC_TRY,ETH_TRY -out venuecheck-report.json
```

Hesap bayraklarını da ölçmek için (yalnız okuma yetkili anahtar):

```sh
mkdir -p ~/.config/trader
# Dosyayı bir editörle oluştur; anahtarı terminal geçmişine yazma:
#   {"api_key": "...", "secret_key": "..."}
chmod 600 ~/.config/trader/binance_tr.json
go run ./cmd/venuecheck -credentials ~/.config/trader/binance_tr.json -out venuecheck-report.json
```

Dosya grup veya diğer kullanıcılar tarafından okunabiliyorsa komut çalışmaz. Ayrıntılar: [docs/exchanges/binance-tr.md](docs/exchanges/binance-tr.md).

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
