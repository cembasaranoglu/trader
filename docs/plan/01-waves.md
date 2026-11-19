# Dalga planı v2 (2026-11-19)

Bu dosya kalan tüm işleri sırasıyla ve kodlanabilir ayrıntıda tanımlar. Kararlar (D-1..D-17) ve değişmezler (I-1..I-7) [plan dokümanında](https://claude.ai/artifact/7Rgd1jjuXpecY2bMzy1NAH) ve `docs/adr/` altındadır. "Üretime hazır" tanımı ve uygulama rolleri [00-overview.md](00-overview.md) içindedir.

Etiketler: **fact** (kaynaklı ya da ölçülmüş), **decision** (kullanıcı kararı), **recommendation** (onay bekler; onaylanmadan o dalgada kodlanmaz), **unknown**, **blocker**, **decision needed** (dalga başlamadan kullanıcıdan gelmeli).

Kaynak kısaltmaları:

- `GH/<dosya>`: resmi Binance dokümanı, `github.com/binance/binance-spot-api-docs` (master, son commit 2026-11-18). Ayrıntı ve satır alıntıları: proje dokümanı `research/E_binance_impl_facts.md`.
- `ÖLÇÜM-1`: venuecheck, kullanıcının makinesi, 2026-11-19 07:20 UTC (`docs/exchanges/binance.md`).
- `ÖLÇÜM-2`: `binance-reachability-probe` iş akışı, GitHub-hosted runner, 2026-11-19 07:47 UTC.

## 1. Mevcut durum (fact, commit `bc7c3e3`)

| Alan | Durum | Kanıt |
| --- | --- | --- |
| Config | Katı yükleyici: bilinmeyen anahtar, eksik alan, NaN/Inf, taşma, ondalık→tam sayı kesilmesi, YAML 1.1 boolean reddi; alanlar arası kurallar; yalnız `binance`/`USDT` ve `mode: paper` | `internal/config`, 60+ tablo testi |
| binance.com REST (salt okuma) | Sunucu saati + `SyncTime` ofseti, exchangeInfo filtreleri (PRICE_FILTER sıfır = devre dışı, NOTIONAL önceliği, maxNotional), 24h ticker, depth, klines, hesap, anahtar yetkileri; https zorunlu, yönlendirme reddi, gövde sınırı, null gövde reddi, 429/418 geri çekilme (1 sn–72 sa) | `internal/exchange/binance`, `internal/exchange/httpx` |
| Binance TR | Yalnız `venuecheck` tanı aracı (ADR-005) | `internal/exchange/binancetr` |
| venuecheck | Paylaşılabilir JSON rapor; kimlik dosyası sertleştirmesi; rapor dizgileri temizlenir | `cmd/venuecheck`, `internal/venuecheck` |
| CI | gofmt, mod tidy, vet, staticcheck v0.8.2, govulncheck v1.10.0 ("No vulnerabilities found"), `test -race`, build, örnek config, şema senkronu; aksiyonlar SHA ile sabit | `.github/workflows/ci.yml`, run 44482344880 yeşil |
| Go | `go 1.26.0`, `toolchain go1.26.11` | `go.mod` |
| Emir, WebSocket, depolama, LLM | **Yok** | — |

### Ölçülmüş erişilebilirlik (ÖLÇÜM-2, fact)

| Uç | GitHub runner'dan HTTP kodu |
| --- | --- |
| `data-api.binance.vision/api/v3/time` | 200 |
| `data.binance.vision/` | 200 |
| `api.binance.com/api/v3/time` | **451** |
| `demo-api.binance.com/api/v3/time` | **451** |
| `testnet.binance.vision/api/v3/time` | **451** |

Sonuç: imzalı uçlara, Demo Mode'a ve testnet'e dayanan her test GitHub Actions'ta **çalışamaz**. Bu testler `//go:build live` etiketiyle ayrılır ve kullanıcının makinesinde ya da VPS'te çalıştırılır (her dalgadaki "Canlı doğrulama" satırı). CI yalnız birim testlerini ve piyasa-verisi-host'una dayanmayan testleri çalıştırır.

## 2. Plan v1'e göre değişenler

| Konu | v1 | v2 | Neden |
| --- | --- | --- | --- |
| Borsa | Binance TR, TRY | binance.com, USDT (D-17) | Kullanıcının hesabı |
| Kullanıcı veri akışı | listen-token / listenKey | WebSocket API `userDataStream.subscribe.signature` | REST listenKey uçları 2026-02-20'de kaldırıldı (fact, GH/CHANGELOG.md 2026-01-21) |
| Test ortamı | "testnet yok" | Demo Mode (`demo-api.binance.com`, 2026-01-29'dan beri) emir yolunun entegrasyon testi için; paper kapısı yine simülatörle | Demo fiyatları canlıya "benzer", eşit değil (fact, GH/demo-mode/general-info.md) |
| Backtest ve A5 | Python | Go replay motoru `internal/features` ve `internal/signal` kodunu doğrudan kullanır; A5 Go'da; Python yalnız istatistik (CPCV/DSR/PBO) | İki dilde aynı gösterge kodunun sapma riskini ortadan kaldırır (recommendation; U-15 onayı ve ADR-001 güncellemesi gerekir) |
| Baseline | BTC/TRY al-tut | BTC/USDT al-tut | D-17 |
| R-21 | USD/TRY şoku | USDT/USD depeg koruması | D-17 |
| W1 | Tek dalga | W1a (tamam), W1b, W1c | Kapsam büyüklüğü |
| W2 | Tek dalga | W2a, W2b, W2c | Kapsam büyüklüğü |
| W9 BtcTurk | Plan içinde | Ertelendi; başlamadan önce yeni ADR gerekir | ADR-005: tek borsa |
| Emir yolu | Genel ilkeler | Bağlayıcı E-1..E-13 kuralları (§5) | Bağımsız plan incelemesi: çift emir ve korumasız pozisyon senaryoları |
| Canlı mod | Config kabul ediyordu | `live_small`/`live_full` W8'e kadar reddedilir | ADR-004 |

## 3. Sıra ve bağımlılıklar

```text
W1b piyasa verisi WS ─┐
W1c depolama + ön kontrol ─┼─> W2a deterministik çekirdek ─> W2b yürütme ─> W2c paper + kill switch ─┬─> W4 LLM (shadow) ─> W5 A3/A4 ─> W6 öğrenme ─> W7 soak + G1 ─> W8 küçük canlı + G2 ─> W10
                           │                                                                          └─> W3 araştırma (W2a'dan sonra paralel; W5'ten önce bitmeli)
```

| Dalga | Teslimat | Bağımlılık | Kapı |
| --- | --- | --- | --- |
| W1a | REST salt okuma, venuecheck, sertleştirme | — | **tamam** |
| W1b | WS piyasa verisi, yerel emir defteri, tazelik izleme, 24 saat observe | W1a | — |
| W1c | SQLite depolama, olay günlüğü (hash zinciri), yedek, başlangıç ön kontrolleri | W1a | — |
| W2a | Evren seçici, özellikler, yer tutucu sinyal, boyutlandırma, risk motoru R-1..R-24 (saf fonksiyonlar) | W1b, W1c | — |
| W2b | Emir yolu, kullanıcı veri akışı, emir durum makinesi, borsada duran stoplar, mutabakat; Demo Mode doğrulaması | W2a | — |
| W2c | Paper simülatörü, orkestratör, kill switch süreci, alarm, gözlem, systemd; 7 gün paper | W2b | — |
| W3 | Veri arşivi, Go replay/backtest, Python CPCV/DSR/PBO, baseline'lar, kural seçimi | W2a | — |
| W4 | LLM katmanı, A1 + A2 shadow, maliyet valisi, politika paketi | W2c | — |
| W5 | A3 + A4 paper karar yolunda, W3 kurallarının taşınması, rol katkı ölçümü | W4, W3 | — |
| W6 | Öğrenme hattı (A5, karantina, few-shot, terfi/geri alma) | W5 | — |
| W7 | Paper soak, VPS sertleştirme, yedek/geri yükleme, kill tatbikatı, G1 raporu | W6 | G1 |
| W8 | Küçük sermaye canlı (200 USD), G2 raporu | G1 onayı | G2 |
| W10 | Tam sermaye işletim | G2 onayı | — |
| W9 | BtcTurk | Yeni ADR | Ertelendi |

## 4. Kullanıcıdan gelecek kararlar ve eylemler (dalgaya göre)

Bir dalga, kendi satırındaki "decision needed" maddeleri kapanmadan başlamaz. "recommendation" sütunu benim önerimdir; onay olmadan uygulanmaz.

| # | Ne | Dalga | Öneri (recommendation) | Durum |
| --- | --- | --- | --- | --- |
| U-1 | `venuecheck` / `observe` / `preflight` / `demo-check` komutlarını kendi makinende ya da VPS'te çalıştırıp çıktıyı paylaşmak | Her dalga | — | sürekli eylem |
| U-2 | WebSocket bağımlılığı. Standart kütüphanede WS istemcisi yok | W1b | `github.com/coder/websocket` v1.8.15 (bağımlılıksız, `go 1.23`, context destekli; fact: proxy.golang.org) | **decision (2026-11-19): onaylandı** |
| U-3 | SQLite sürücüsü | W1c | `modernc.org/sqlite` v1.62.0 (saf Go, cgo yok; `modernc.org/libc` v1.80.1 birebir sabitlenir; OFD kilitleri koddan açılır). Alternatif `mattn/go-sqlite3` v1.14.53 (cgo + gcc) | **decision (2026-11-19): onaylandı** (ADR-006) |
| U-4 | VPS sağlayıcısı ve bölgesi. Binance'in kısıtladığı bir konum olmamalı (ABD IP'leri 451, ÖLÇÜM-2; tam ülke listesi API dokümanında yok) | W2c | Bölge seçilince `venuecheck` VPS'te çalıştırılır | **decision needed** |
| U-5 | İlk kripto evren izin listesi (D-1, D-4). API varlık sınıfını ayırt etmiyor | W2b başı (W2a'nın `traderd universe` çıktısıyla) | Aday listeyi W2a üretir | **decision needed** |
| U-6 | İki ayrı Demo Mode API anahtarı (demo.binance.com): biri traderd, biri kill switch için. Demo'nun hangi anahtar tiplerini desteklediği unknown; Ed25519 denenir, olmazsa HMAC | W2b | — | eylem |
| U-7 | Alarm kanalı: Telegram botu (token + chat id). Telegram'dan elle kill komutu verebilecek tek chat id | W2c | Komutlar yalnız tanımlı chat id'den, `cancel_entries` / `close_all`, onay ikinci mesajla | **decision needed** |
| U-8 | Haber kaynakları beyaz listesi ve `golang.org/x/text` v0.44.0 bağımlılığı (Unicode normalizasyonu). Resmi kaynak: Binance Announcements WebSocket (yalnız HMAC imzası belgelenmiş) | W2c (yalnız Binance duyuruları, R-16 için), W4 (diğer kaynaklar) | Announcements için ayrı, yalnız-okuma HMAC anahtarı | **decision needed** |
| U-9 | Anthropic API anahtarı, workspace harcama limiti, `github.com/anthropics/anthropic-sdk-go/v2` v2.11.0 bağımlılığı | W4 | Resmi SDK (fact: Go 1.25+, v2.11.0 2026-11-17) | **decision needed** + eylem |
| U-10 | VPS dışı heartbeat izleyicisi: yalnız alarm mı, iptal yetkili ayrı anahtar mı | W7 | Yalnız alarm (ek anahtar yüzeyi açmamak için) | **decision needed** |
| U-11 | Canlı anahtarlar: traderd (Spot trading + Reading, IP kısıtlı, çekim kapalı), kill switch (aynı yetkiler, ayrı anahtar) | W7 | Ed25519 | eylem |
| U-12 | G2 eşikleri. Süre kararlı (D-16: 180 gün); güven düzeyi, bootstrap yöntemi ve blok uzunluğu, baseline seti kayıtlı değil | W8 başı | G1 ile aynı yöntem (U-17) | **decision needed** |
| U-13 | Sık işlemin vergi niteliği (GVK Md. 37), SMMM/YMM görüşü | W8 öncesi | — | dış danışmanlık |
| U-14 | Komisyon varlığı: BNB ile ödeme (indirimli, R-19'da ayrı kural) mı, alınan varlıktan kesilmesi mi | W2b | BNB **kullanılmaz**: ücret alınan varlıktan düşer, koruma miktarı net miktardan hesaplanır; BNB fiyat riski ve ayrı mutabakat kuralı gerekmez | **decision needed** |
| U-15 | Backtest motorunun ve A5 çağrısının Go'ya taşınması (ADR-001 katman 4 ve D-8 "Python araştırma" tanımını değiştirir); Python yalnız istatistik | W3 | Onay (gösterge kodunun tek kaynağı olması için) | **decision needed** |
| U-16 | Koruyucu stop tipi: `STOP_LOSS` (tetiklenince MARKET; fact: GH/rest-api.md) ya da `STOP_LOSS_LIMIT` (boşlukta dolmayabilir) | W2b | `STOP_LOSS` | **decision needed** |
| U-17 | G1 eşikleri (önceden kayıt): bootstrap güven düzeyi, yöntem ve blok uzunluğu, maliyet doğruluğu toleransı, baseline seti (BTC/USDT al-tut, momentum, eşit ağırlıklı sepet) | W2c sonu (paper verisi toplanmaya başlamadan kaydedilir) | %95 tek yönlü, sabit 7 günlük blok bootstrap | **decision needed** |
| U-18 | USD/TRY kur kaynağı (vergi defteri) ve USDT/USD depeg referansı (R-21) | W2c | Birer resmi/kamu kaynak; seçim kullanıcıda | **decision needed** |
| U-19 | Onay imzası: kullanıcı onayları (kill sonrası yeniden başlatma, politika terfisi, kapı) hangi anahtarla | W2c | OpenSSH `ssh-keygen -Y sign` ile kullanıcının SSH anahtarı; doğrulama `allowed_signers` dosyasıyla | **decision needed** |
| U-20 | Python araç zinciri `uv` ve kilitli bağımlılıklar (numpy, pandas, scipy); yedek şifreleme için `age` | W3, W7 | Onay | **decision needed** |
| U-21 | Plan v2'deki küçük biçim kararları: CSV (Parquet değil), OTel exporter yok (gen_ai.* adları log/metrikte) | W3, W5 | Onay | **decision needed** |
| U-22 | W7 canlı kill tatbikatı için harcanabilecek tutar ve ücret | W7 | En küçük geçerli emir (5 USDT minNotional üstü), tatbikat başına ≤ 15 USDT | **decision needed** |
| U-23 | Veritabanı: kullanıcı SQLite'ı reddetti (2026-11-19). PostgreSQL ve barındırma şekli (bulut sağlayıcının yönetilen PostgreSQL'i / VPS üzerinde kurulu PostgreSQL). Emir yolu (E-2) her niyette commit beklediği için veritabanı VPS ile aynı sağlayıcı ve bölgede olmalı (U-4 ile birlikte karar). Araştırma: proje dokümanı `research/F_database_options.md` | W1c yeniden (W2a'dan önce) | Sağlayıcıdan bağımsız standart PostgreSQL ≥ 16, `pgx/v5` v5.13.0; barındırma VPS ile aynı bölgede yönetilen PostgreSQL; altyapı maliyeti kapı hesabına eklenir | **decision needed** |

## 5. Dalga spesifikasyonları

Her dalga için ortak çalışma protokolü (00-overview.md rolleri):

1. Orkestratör dalga spesifikasyonunu bu dosyadan alır; o dalganın "decision needed" maddeleri kapalı değilse dalga başlamaz.
2. **Test tasarımcısı** ajanı spesifikasyondan, koda bakmadan, kenar durum ve hata senaryosu listesi üretir; liste `docs/plan/tests/<dalga>.md` olarak commit'lenir ve her madde bir testle eşleşir.
3. Orkestratör kodlar; her paket için tablo testleri, gerektiğinde fuzz testleri yazar.
4. **Kod inceleyici** ajanı dalga sonunda bağımsız inceleme yapar. Bulgular düzeltilmeden ve yeniden doğrulanmadan dalga kapanmaz.
5. Doğrulama komutları çalıştırılır, çıktı özeti commit mesajına yazılır. Çalıştırılamayan komut "not run" olarak işaretlenir.
6. Kullanıcı canlı doğrulama komutunu çalıştırır; çıktı `docs/exchanges/` ya da dalga notuna işlenir.

Ortak doğrulama komutları (her dalgada, depo kökünde):

```sh
gofmt -l .                                              # boş olmalı
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
go test -race -count=1 ./...
go build ./...
go run ./cmd/traderd -config configs/example.yaml      # alt komutlar gelince de geçerli kalır (validate takma adı)
```

### Emir yolu tasarım kuralları (W2a–W2c için bağlayıcı)

Bu kurallar doğrulayıcı incelemesindeki yüksek önemli bulgulardan çıktı. Kod ve testler bunlara göre yazılır.

| # | Kural | Gerekçe (kaynak) |
| --- | --- | --- |
| E-1 | **Intent ID deterministiktir:** `SHA-256(policy_version ‖ symbol ‖ signal_kind ‖ kapanmış mumun kapanış zamanı)`; her alan 4 baytlık uzunluk önekiyle kodlanır (ör. `("v1","BTCUSDT")` ile `("v1B","TCUSDT")` çakışmaz), zaman UTC Unix milisaniye ondalık dizgi. Depoda `UNIQUE`; aynı sinyalin yeniden başlatmadan sonra yeniden üretilmesi yeni intent oluşturmaz | Çift emir önleme (R-12) |
| E-2 | **Önce yaz, sonra gönder:** intent, client order ID, hedef stop ve kar al fiyatları emir gönderilmeden önce depoya yazılır (fsync). Gönderim sonucu ayrı olay | Çökme sonrası korumasız pozisyon (bulgu 7) |
| E-3 | **Aynı client order ID asla yeniden gönderilmez.** Binance aynı ID'yi yalnız önceki emir açıkken reddeder; dolmuş bir emrin ID'siyle yeni emir kabul edilir (fact: GH/rest-api.md "unique id among open orders… accepted only when the previous one is filled"). `-2010 Duplicate` yalnız açık emir için koruma sağlar | Bulgu 1 |
| E-4 | **Giriş emrinde UNKNOWN sonrası yeniden deneme yok.** 5XX, `-1007`, zaman aşımı ya da bağlantı kopmasında intent `UNKNOWN` olur; `GET /api/v3/order` (origClientOrderId) ve `myTrades` sorgulanır. Bulunamazsa intent `ABANDONED_UNKNOWN` olur, o sembolde en az `ops.unknown_block_cycles` mutabakat döngüsü yeni giriş yok; geç ortaya çıkan emir client ID önekinden tanınıp sahiplenilir. Not: "sorguda bulunamadı = gerçekleşmedi" Binance tarafından garanti edilmez (fact: CHANGELOG.md'de recvWindow kontrolünün eşleştirme motoru içindeki gecikmeyi kapsamadığı belirtiliyor); tasarım bu varsayıma dayanmaz | Bulgu 2 |
| E-5 | **Client order ID biçimi:** `<rol><bacak><sıra2>-<base32(SHA-256(intent_id))[:26]>`; rol `e` giriş, `p` koruma, `x` çıkış, `k` kill switch'in yerleştirdiği koruma ya da kapatma (E-10 ve E-12 `k` önekli OCO'yu koruma sayar); bacak `m` tek emir, `l` OCO liste, `a` OCO üst, `b` OCO alt; sıra 00–99 (yeni koruma/çıkış denemesi için; giriş için yalnız 00). Uzunluk 31 karakter, karakter kümesi `[A-Za-z0-9-]` (GH/fix-api.md regex'inin alt kümesi; REST kuralı belgelenmemiş, W2b'de Demo'da ölçülür) | Bulgu 5 |
| E-6 | **Giriş yalnız IOC LIMIT:** tek nihai durum (doldu / kısmi doldu + süresi doldu / dolmadı), dolayısıyla tek koruma emri. GTC girişi yok (çoklu kısmi dolum, MAX_NUM_ALGO_ORDERS sınırı) | Bulgu 4 |
| E-7 | **Koruma miktarı net miktardır:** Σ`l` − Σ`n` (yalnız `N` = baz varlık olan dolumlarda) − o pozisyondan yapılmış çıkışlar, `StepSize`'a aşağı yuvarlanır. Kaynak, depodaki sistem pozisyon kaydıdır (E-10); serbest bakiye yalnız üst sınır kontrolüdür, miktar kaynağı değildir | Bulgu 3 (GH/user-data-stream.md `n`, `N`) |
| E-8 | **Minimum giriş büyüklüğü:** `sizing.min_notional_multiple` × `minNotional` altındaki giriş boyutlandırmada sıfırlanır; ücret ve yuvarlama sonrası koruma emri ve piyasa çıkışı her zaman `minNotional` / `minQty` üstünde kalır. Yine de altında kalan miktar `DUST` durumudur: alarm, mutabakatta izlenir, yeni girişi engellemez | Bulgu 4 |
| E-9 | **Koruma:** IOC nihai durumundan hemen sonra SELL OCO (üst `LIMIT_MAKER`, alt U-16'daki tip). OCO reddedilirse bir kez yeni sıra ile denenir, yine olmazsa piyasa SELL ile kapatılır. Kapatma da olmazsa alarm + o sembolde yeni giriş yok + her mutabakat döngüsünde yeniden denenir | R-24 |
| E-10 | **Başlangıçta korumasız pozisyon taraması:** tarama evrene değil, depodaki **sistem pozisyonlarına** dayanır (kendi `e` dolumlarından türetilen, kapanmamış pozisyonlar; evrenden düşmüş semboller dahil). Her açık sistem pozisyonu için canlı koruma (`p` ya da `k` önekli OCO) yoksa: kayıtlı stop/kar al fiyatlarıyla, yalnız **kayıtlı miktar** kadar OCO; stop fiyatı geçilmişse kayıtlı miktar kadar piyasa SELL. Hesaptaki fazla bakiye sistem dışıdır: dokunulmaz, bilgi alarmı verilir. Bakiye kayıtlı miktarın altındaysa mutabakat farkıdır (R-19) | Bulgu 7, doğrulama N1 |
| E-11 | **Çıkış ve close_all sırası:** `DELETE /api/v3/orderList` ile OCO iptali (tek bacağın iptali tüm listeyi iptal eder; fact: GH/rest-api.md) → piyasa SELL (net miktar) → SELL reddedilirse OCO'yu yeni sıra ile geri yerleştir. Korumasız pencere bu iki çağrı arasıyla sınırlıdır ve olay günlüğüne süresiyle yazılır | Bulgu 9 |
| E-12 | **Kill switch hiçbir zaman `DELETE /api/v3/openOrders` kullanmaz** (OCO'ları da iptal eder). Heartbeat kaybında yalnız `e` önekli açık emirleri tek tek iptal eder; `p` ve `k` koruma emirlerine dokunmaz. Önek tanınmayan emirlere dokunmaz, alarm verir | Bulgu 9 |
| E-13 | **İptal ve koruma emirleri hiçbir yerel frende beklemez:** rate-limit geri çekilmesi host ve sınıf bazında ayrıdır (piyasa verisi hostu, imzalı host, `-1015` emir sayısı); saat sapması (R-20) ve emir hızı (R-10) yeni girişleri durdurur, iptal ve koruma emirlerini durdurmaz | Bulgu 6, 33 |

---

### W1b — WebSocket piyasa verisi ve yerel emir defteri

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Evrendeki semboller için kesintisiz, doğrulanmış, tazeliği ölçülen piyasa verisi: kapanmış mumlar, en iyi fiyat, işlemler (aggTrade, simülatör için), yerel emir defteri |
| 2 | Kaynak | GH/web-socket-streams.md: hostlar, `/stream?streams=`, küçük harf sembol, bağlantı ≤ 24 sa, sunucu 20 sn'de bir ping, 1 dk içinde pong yoksa kopar, bağlantı başına 5 mesaj/sn, 1024 akış, IP başına 5 dk'da 300 bağlantı, `serverShutdown`, yerel defter prosedürü. GH/rest-api.md depth ağırlığı: 1–100 → 5, 101–500 → 25, 501–1000 → 50, 1001–5000 → 250. ÖLÇÜM-2: `data-api.binance.vision` erişilebilir |
| 3 | Kapsam | (a) `internal/exchange/binance/ws`: combined stream istemcisi, host `wss://data-stream.binance.vision`; akışlar `@kline_<i>`, `@bookTicker`, `@depth@100ms`, `@aggTrade`. Ping'e aynı payload ile pong (kütüphane yapar); bekçi: veri ya da sunucu ping'i olmadan 60 sn geçerse bağlantı yenilenir (sessiz piyasada bağlantı korunur); `serverShutdown` → yeni bağlantı açılır, yeni bağlantı mesaj teslim edince eski kapatılır; 23 sa 50 dk'da planlı devir (24 sa sınırı dokümanda `stream.binance.com` için yazılı, `data-stream.binance.vision` için savunmacı olarak aynısı uygulanır); üstel geri çekilmeli yeniden bağlanma (1 sn → `marketdata.reconnect_max_backoff_s`, jitter [d/2, d]); IP başına bağlantı denemesi bütçesi (300/5 dk'nın yarısı). Abonelik URL ile yapılır, istemci kontrol mesajı göndermez; bu yüzden giden mesaj sınırlayıcısı gerekmez (yalnız sunucu ping'lerine pong gider, 20 sn'de bir). Akış adındaki sembol/aralık ile olay içeriği uyuşmazsa mesaj bozuk sayılır. `encoding/json` anahtarları büyük/küçük harf duyarsız eşlediği için belgelenen her anahtar (`m`/`M`, `l`/`L` gibi yok sayılanlar dahil) açıkça bildirilir. (b) `internal/marketdata/book`: GH prosedürüyle yerel defter; snapshot `marketdata.depth_limit` (örnek değer 1000 → ağırlık 50); aynı anda en fazla 2 snapshot isteği; snapshot defteri canlıya geçirmezse (eski, boşluk, hata) yeniden istek 200 ms'den 5 sn'ye üstel gecikmeyle (REST snapshot'ı akışın gerisinde kalırsa sıkı döngü olmaz). Canlı akışta `u < yerel id` yok sayılır, `u == yerel id` dokümana göre uygulanır (miktarlar mutlak olduğu için sonuç değişmez). Fiyat/miktar defterde `int64` sabit nokta tutulur; ölçek sembolün tick/step ondalık basamağıdır, `TickSize`/`StepSize` nil (0 = devre dışı) ise snapshot dizgilerindeki en büyük ondalık basamak sayısı; ölçeği aşan bir değer gelirse ölçek büyütülerek defter yeniden kurulur; `int64` taşması ölçek büyütmeyle düzelmeyeceği için defteri `Failed` durumuna alır (sembol stale kalır, yeniden kurma döngüsü olmaz); dışarıya `*big.Rat` verilir (5000 seviyede `big.Rat` maliyetinden kaçınmak için). (c) Kline: yalnız `x=true` mumlar yayınlanır; boşluk → REST `klines` (`startTime`/`endTime`, 1000'lik sayfalar) ile doldurma; doldurulamayan boşluk sonraki mumda `GapBefore` olarak işaretlenir. (d) `internal/marketdata/trades`: aggTrade yayını (W2c simülatörü). (e) `internal/marketdata/freshness`: sembol başına son olay zamanı; `risk.max_data_age_ms` aşımı → stale (R-9). (f) REST ağırlık bütçesi: `internal/exchange/binance` istemcisine host bazlı ayrı geri çekilme ve dakikalık ağırlık sayacı (yanıttaki `X-MBX-USED-WEIGHT-1M`); ağırlığın `%20`'si emir/iptal yoluna ayrılır (E-13). (g) `cmd/traderd` alt komutları: `validate` (varsayılan; `traderd -config f` eski biçim aynen çalışır), `observe` |
| 4 | Kapsam dışı | İmzalı akışlar, emir, depolama (W1c), evren seçimi (W2a; W1b komut satırındaki sembol listesiyle çalışır) |
| 5 | Ön koşul | U-2 |
| 6 | Varsayımlar | Yok. `serverShutdown` sıklığı ve gerçek gecikme observe ile ölçülür |
| 7 | Blocker | U-2 |
| 8 | İncelenecek | `internal/exchange/binance/*`, `internal/exchange/httpx` |
| 9 | Eklenecek | `internal/exchange/binance/ws/{stream.go,events.go,*_test.go}`, `internal/fixedpoint/*`, `internal/marketdata/{marketdata.go,convert.go,book/*}`, `cmd/traderd/{validate.go,observe.go}` |
| 10 | Değişecek | `internal/exchange/binance/client.go` (host bazlı geri çekilme, ağırlık sayacı; mevcut testler korunur), `cmd/traderd/main.go`, `internal/config` (`marketdata.*`), `configs/example.yaml`, şema, `go.mod`, README |
| 11 | Dokunulmayacak | Mevcut config doğrulama kuralları (yalnız ekleme) |
| 12 | Çalışma yüzeyi | `traderd observe -config <f> -symbols BTCUSDT,ETHUSDT -duration 24h -out observe.jsonl` |
| 13 | Sözleşmeler | `marketdata.BookView` (N seviye, `LastUpdateID`, durum ve `Stale` tek kilit altında), `marketdata.Bar` (yalnız kapanmış mum; `FromREST`, `GapBefore`), `marketdata.Trade` (aggTrade kimliği, ilk/son işlem kimliği), `marketdata.Ticker`; `marketdata.Sink` sırayla çağrılır |
| 14 | Config/doküman | Zorunlu: `marketdata.depth_limit` (1..5000), `marketdata.kline_intervals` (beyaz liste enum), `marketdata.reconnect_max_backoff_s` (1..300). `docs/exchanges/binance.md` WS bölümü |
| 15 | Testler | Sahte WS sunucusu (`httptest` + kütüphanenin `Accept`'i): ping/pong; 60 sn sessizlikte kopma; `serverShutdown` ile kesintisiz devir; 24 sa devri (sahte saat); defter: snapshot eski → yenile, `u <= lastUpdateId` atılır, boşluk → yeniden kur, sıfır miktar silme, çapraz defter → yeniden kur, eşzamanlı yeniden kurma sınırı; kline: açık mum yayınlanmaz, boşluk → REST; bozuk JSON / bilinmeyen olay / büyük mesaj → bağlantı yenilenir; host bazlı geri çekilme: piyasa hostundan 429 imzalı hostu engellemez; context iptalinde tüm goroutine'ler `WaitGroup` ile zaman sınırında biter. Fuzz: olay ayrıştırıcı, sabit nokta dönüşümü |
| 16 | Doğrulama | Ortak komutlar |
| 17 | Build | `go build ./cmd/traderd` |
| 18 | Canlı doğrulama | `go run ./cmd/traderd observe -config configs/example.yaml -symbols BTCUSDT,ETHUSDT,SOLUSDT -duration 24h -out observe.jsonl`; son özet satırı paylaşılır |
| 19 | Riskler | Sessiz veri kaybı → boşluk kontrolü + tazelik; IP banı → bağlantı ve ağırlık bütçesi |
| 20 | Geri alma | Salt okuma |
| 21 | Çıkış kriteri | Testler + inceleme temiz; 24 sa observe: kapanmış mum kaybı 0, tüm defter tutarsızlıkları kurtarılmış, stale süresi raporlandı |
| 22 | Sonraki adım | W1c (paralel kodlanabilir) |

### W1c — Depolama, olay günlüğü, yedek, ön kontroller

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Kararların, emirlerin, dolumların, onayların çökme güvenli ve değiştirilemez kaydı (I-6); başlamadan önce güvenlik koşullarının borsadan doğrulanması (I-7, R-20, R-22) |
| 2 | Kaynak | ADR-002; `research/E_binance_impl_facts.md` §J; `/sapi/v1/account/apiRestrictions` (`internal/exchange/binance/endpoints.go`; Binance wallet dokümanı, `docs/exchanges/binance.md`) |
| 3 | Kapsam | (a) `internal/store`: tek yazıcı bağlantı (okuma havuzu ihtiyaç W2c'de ölçülürse eklenir); WAL, `busy_timeout`, `synchronous=FULL`, `foreign_keys`, `_txlock=immediate`; dosyalar `0600`; gömülü ileri-yalnız migration'lar. (b) Tablolar (STRICT): `events` (append-only: `seq`, `ts_ns`, `kind`, `payload`, `prev_hash`, `hash` = SHA-256(prev_hash ‖ seq ‖ ts_ns ‖ kind ‖ payload), sabit genişlikli tam sayılar ve uzunluk önekli dizgilerle; tetikleyiciler UPDATE/DELETE'i ve `seq ≠ max+1` eklemeyi reddeder, `recursive_triggers=ON` ile `INSERT OR REPLACE` de geçemez; zincir ucu her commit sonrası veritabanı dışındaki çapa dosyasına atomik yazılır, böylece kuyruk kesme ve eski kopya tespit edilir; payload `"v"` sürüm alanlı JSON nesnesi), `intents` (E-1, E-2; `UNIQUE(intent_id)`), `orders`, `fills`, `balances_snapshot`, `approvals`, `tax_ledger` (zaman, sembol, yön, miktar, fiyat, ücret, ücret varlığı, USD/TRY kuru ve kaynağı; işlem vergisi oranı alanı backtest için). (c) `store.VerifyFile` / `Store.Verify`: koruyucu tetikleyicilerin varlığı + zincir + çapa, sabit bellekle; uygulanmış migration içeriği SHA-256 ile korunur. (d) Yedek: `VACUUM INTO` + SHA-256 (şifreleme ve VPS dışı W7). (e) `internal/preflight`: config → `SyncTime` (R-20; |ofset| + gidiş-dönüş/2 ≤ sınır) → `KeyPermissions`: çekim açık / `OtherEnabled` dolu / işlem yetkisi IP kısıtsız → başlama (I-7) → `marketdata`/komut satırındaki sembollerde `allowedSelfTradePreventionModes` ∌ `execution.stp_mode` → başlama (R-22; W2a evrene genişletir) → zincir doğrulama. (f) Alt komutlar: `preflight`, `store verify`, `store backup` |
| 4 | Kapsam dışı | Emir verisi üretimi (W2b), şifreli yedek taşıma (W7) |
| 5 | Ön koşul | U-3 |
| 6 | Varsayımlar | Yok. `/sapi` uçlarının Demo Mode'da çalışıp çalışmadığı unknown (testnet'te çalışmadığı belgeli; E §D) → yetki adımı yalnız `-environment demo` açıkça verildiğinde atlanır ve raporda SKIP yazar; çağrının hata vermesi hiçbir zaman atlama sebebi değildir |
| 7 | Blocker | U-3 |
| 8 | İncelenecek | ADR-002, `internal/exchange/binance` |
| 9 | Eklenecek | `internal/store/{store.go,migrations/0001_init.sql,store_test.go}`, `internal/preflight/*`, `internal/credentials/*` (venuecheck'ten taşındı), `cmd/traderd/preflight.go`, `docs/adr/006-sqlite-driver.md`, `docs/runbook/storage.md` |
| 10 | Değişecek | `go.mod`, `cmd/traderd`, config (`store.path`, `store.backup_dir`), şema, README; CI: libc sürüm eşleşme kontrolü (modernc seçilirse) |
| 11 | Dokunulmayacak | `internal/exchange/*` sözleşmeleri |
| 12 | Çalışma yüzeyi | `traderd preflight -config <f> -credentials <f> -symbols A,B [-environment live\|demo]`, `traderd store verify -config <f> \| -file <db>`, `traderd store backup -config <f>` (salt okuma; çalışan süreçle güvenli) |
| 13 | Sözleşmeler | Olay `kind` (`^[a-z][a-z0-9_]{0,63}$`) + payload sürümü `v` (zorunlu tam sayı ≥ 1); saklanan baytlar hash'lenen baytlardır (kanonik JSON gerekmez: doğrulama baytları yeniden hash'ler); preflight her ret nedeni için tipli hata döner (`ErrClockSkew`, `ErrWithdrawalsEnabled`, ...) ve tüm kontrolleri çalıştırıp raporlar |
| 14 | Config/doküman | `store.path`, `store.backup_dir` |
| 15 | Testler | Migration ileri/çift; trigger reddi; zincir kurcalama (payload, sıra, silme); alt süreç `SIGKILL` sonrası tutarlılık; yedeğin açılıp doğrulanması; `intents` UNIQUE ihlali; preflight: her ret nedeni ayrı tipli hata |
| 16 | Doğrulama | Ortak komutlar + `CGO_ENABLED=0 go build ./...` (modernc ise) |
| 17 | Build | Yukarıdaki |
| 18 | Canlı doğrulama | `go run ./cmd/traderd preflight -config configs/example.yaml -credentials ~/.config/trader/binance.json` (yalnız okuma anahtarı) |
| 19 | Riskler | libc sürüm uyumsuzluğu → CI kontrolü; OFD kilidi uyarısı → systemd'de `MODERNC_SQLITE_OFD_LOCK=1` (W2c); disk dolması → yazma hatası güvenli moda geçirir (W2c) |
| 20 | Geri alma | İleri-yalnız migration; veri yedekten |
| 21 | Çıkış kriteri | Testler + inceleme temiz; kullanıcının preflight çıktısı |
| 22 | Sonraki adım | W2a |

### W2a — Deterministik çekirdek (saf fonksiyonlar)

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Sayı üreten ve karar veren her şeyin (I-2) G/Ç'siz, test edilebilir saf Go kodu olması |
| 2 | Kaynak | Plan: deterministik bileşenler, R-1..R-24; GH/filters.md (PRICE_FILTER, PERCENT_PRICE_BY_SIDE, LOT_SIZE, MARKET_LOT_SIZE, NOTIONAL, MAX_NUM_ORDERS, MAX_NUM_ALGO_ORDERS, MAX_NUM_ORDER_LISTS); ÖLÇÜM-1 |
| 3 | Kapsam | (a) `internal/exchange` + `binance.parseFilters`: eksik filtreler eklenir (yukarıdaki liste). (b) `internal/decimal`: tick/step'e aşağı/yukarı yuvarlama, biçimleme. (c) `internal/params`: minimal sürümlü parametre dosyası yükleyici (`policies/v0000/params.yaml`, katı YAML + SHA-256); tam politika paketi W4'te bunu genişletir. (d) `internal/universe`: izin listesi (U-5), `SpotEnabled`, quote USDT, min 24h hacim, max spread, min listelenme yaşı, anormal sıçrama dışlama, top N; `traderd universe` alt komutu (salt okuma, aday liste + dışlama nedenleri). (e) `internal/features`: ATR, gerçekleşmiş volatilite, momentum, EMA, spread (float64 yalnız göstergede). (f) `internal/signal`: kural kayıt defteri (registry); yer tutucu momentum kuralı kayıtlı ve etkin; W3 aday kuralları aynı kayıt defterine eklenir ama orkestratöre bağlanmaz. `Intent` E-1 ile kimliklenir. (g) `internal/sizing`: volatilite hedefli boyut; R-4; R-17; E-8 minimum. (h) `internal/risk`: her R için ayrı fonksiyon, tek `Evaluate(State, Intent) Decision`; sıra plan tablosundaki sıra; tüm sonuçlar `Decision.Checks`'te. Kapsanan: R-1..R-11, R-13, R-14 (yuvarlama sonrası tüm filtreler; `TickSize`/`StepSize` nil → red), R-15 (durum ≠ TRADING → yeni emir yok), R-16 (`State.DelistBlocked` bayrağı; kaynağı W2c), R-17, R-18, R-19 (`State.ReconcileOK`), R-20 (`State.ClockOK`; yalnız yeni girişi durdurur, E-13), R-21 (`State.DepegOK`; kaynağı W2c), R-22 (STP modu emirde set, izinli mod kontrolü), R-23 (`State.VenueHalted` + borsa maruziyeti). R-12 ve R-24 yürütmede (W2b). (i) `internal/clock` |
| 4 | Kapsam dışı | Ağ, depolama, emir |
| 5 | Ön koşul | W1b, W1c |
| 6 | Varsayımlar | Yok. Gösterge parametreleri `params` dosyasından; yer tutucu değerler W3 sonuçlarıyla değişir |
| 7 | Blocker | Yok (U-5 W2a'nın çıktısıyla verilir, W2b'yi bloklar) |
| 8 | İncelenecek | `internal/exchange/types.go`, `internal/config` |
| 9 | Eklenecek | `internal/{decimal,params,universe,features,signal,sizing,risk,clock}/*`, `policies/v0000/params.yaml`, `configs/universe-allowlist.yaml` (U-5 sonrası dolar), `cmd/traderd/universe.go`, `docs/risk.md` |
| 10 | Değişecek | `internal/exchange/types.go`, `internal/exchange/binance/endpoints.go` (filtreler), `internal/config` (`universe.*`, `sizing.min_notional_multiple`), şema, `internal/preflight` (STP kontrolünü evrene genişlet) |
| 11 | Dokunulmayacak | REST istemci taşıma kuralları |
| 12 | Çalışma yüzeyi | `traderd universe -config <f>` |
| 13 | Sözleşmeler | `risk.State`, `risk.Decision`, `signal.Intent`, `sizing.Order` |
| 14 | Config/doküman | `universe.{top_n,min_quote_volume_usd,max_spread_bps,min_listing_days,allowlist_path}`, `sizing.min_notional_multiple` (≥ 2); `docs/risk.md`: her R için formül, kaynak alan ve test adı |
| 15 | Testler | Her R için tablo testi (altı, tam sınır, üstü, sıfır, negatif, nil); R-1..R-3 UTC gün/hafta sınırı; R-11 pencere; R-14 yuvarlama fuzz (sonuç ızgarada ve sınırlar içinde ya da red); E-8 (ücret + yuvarlama sonrası çıkış ≥ minNotional); özellikler golden; izin listesinde olmayan sembol asla seçilmez; params SHA-256 uyuşmazlığında yükleme reddi. Kapsam: risk/sizing ≥ %90 |
| 16 | Doğrulama | Ortak komutlar + `go test -cover ./internal/risk/... ./internal/sizing/...` |
| 17 | Build | `go build ./...` |
| 18 | Canlı doğrulama | `go run ./cmd/traderd universe -config configs/example.yaml` → U-5 onayı bu çıktıyla |
| 19 | Riskler | Hatalı yuvarlama → fuzz; sessiz kural atlama → `Decision.Checks` |
| 20 | Geri alma | Kütüphane |
| 21 | Çıkış kriteri | Tüm R kontrolleri testli ve `docs/risk.md` ile eşleşiyor; kapsam eşikleri; inceleme temiz |
| 22 | Sonraki adım | W2b (U-5 kapanınca); W3 paralel başlar |

### W2b — Emir yolu, kullanıcı veri akışı, mutabakat

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Onaylı emrin borsaya en fazla bir kez gitmesi, korumasız pozisyon kalmaması (R-24), defter ile borsanın mutabık olması (R-19); E-1..E-13'ün uygulanması |
| 2 | Kaynak | GH/rest-api.md: `POST /api/v3/order` (+`/test`), `DELETE /api/v3/order`, `GET /api/v3/order`, `GET /api/v3/openOrders`, `GET /api/v3/myTrades` (`fromId`; zaman penceresi en fazla 24 sa), `POST /api/v3/orderList/oco`, `DELETE /api/v3/orderList`, `GET /api/v3/exchangeInfo`; 5XX/`-1007` = sonuç bilinmiyor; `-2010`; GH/web-socket-api.md: `wss://ws-api.binance.com:443/ws-api/v3`, `userDataStream.subscribe.signature` (her anahtar tipi), `serverShutdown`; GH/user-data-stream.md: `executionReport` (`c`, `C`, `X`, `x`, `l`, `z`, `L`, `n`, `N`, `T`, `t`), `outboundAccountPosition`, `balanceUpdate`, `listStatus`, `eventStreamTerminated` (yalnız logout/unsubscribe/token süresinde) |
| 3 | Kapsam | (a) İmzalı POST/DELETE (sorgu imzası), sınıf bazlı geri çekilme (E-13). (b) `internal/execution`: E-1..E-11; durum makinesi `PLANNED → PERSISTED → SUBMITTING → {NEW, PARTIALLY_FILLED, FILLED, CANCELED, REJECTED, EXPIRED, EXPIRED_IN_MATCH, UNKNOWN}`, `UNKNOWN → {sahiplenildi, ABANDONED_UNKNOWN}`; geçiş tablosu kod ve testte tek kaynak. Olay sıralaması kümülatif `z` ve durum önceliğiyle (`I`'nin artan olduğu belgelenmemiş); iptal olaylarında asıl ID `C` alanında. (c) Koruma ve çıkış: E-6..E-11; koruma alt bacağı U-16. (d) Kullanıcı veri akışı: `userDataStream.subscribe.signature`; **her** yeniden bağlanmada yeniden abone + REST mutabakatı; 24 sa devir; `serverShutdown`. (e) `internal/reconcile`: başlangıçta ve `ops.reconcile_interval_s`'de: bakiyeler, açık emirler ve listeler, son kontrol noktasından beri `myTrades` (`fromId` ile); `balanceUpdate` (yatırma/çekme) defter düzeltmesi olarak kaydedilir, fark sayılmaz; fark `reconcile_tolerance_steps` üstündeyse yeni giriş durur, `reconcile_escalate_cycles` sürerse kill isteği (W2c protokolü). U-14 = BNB ise ücret varlığı ayrı toleransla. E-10 başlangıç taraması. (f) `exchangeInfo` periyodik yenileme (`ops.exchange_info_refresh_s`): filtre ve durum değişikliği → R-14/R-15 güncellenir, olay yazılır. (g) Saat senkronu periyodik (`ops.clock_sync_interval_s`). (h) `mode: paper` iken gerçek emir uçları kod düzeyinde çağrılamaz. İstisnalar yalnız iki komuttur: `demo-check` (yalnız demo-api hostu) ve W7'deki `drill` (canlı host, U-22 tutar tavanı, etkileşimli onay); ve `live` etiketli testler (yalnız demo-api). (i) `traderd demo-check`: Demo'da minimum tutarlı IOC giriş → OCO → OCO iptali + piyasa çıkışı → kill switch anahtarıyla iptal denemesi (iki anahtar, K-8) → mutabakat; sonuç raporu |
| 4 | Kapsam dışı | Strateji, simülatör, kill switch süreci (W2c) |
| 5 | Ön koşul | W2a; U-5, U-6, U-14, U-16 |
| 6 | Varsayımlar | Yok. Belgelenmemiş davranışlar Demo'da ölçülür (K-1, K-8) ve tasarım bunlara dayanmaz |
| 7 | Blocker | U-5, U-6, U-14, U-16 |
| 8 | İncelenecek | `internal/exchange/binance/client.go`, `internal/{risk,store}` |
| 9 | Eklenecek | `internal/exchange/binance/{orders.go,orderlist.go,userstream.go,live_test.go}` (`//go:build live`), `internal/execution/*`, `internal/reconcile/*`, `cmd/traderd/democheck.go`, `docs/runbook/order-unknown.md` |
| 10 | Değişecek | `internal/exchange/binance/client.go`, config (`ops.reconcile_interval_s`, `ops.clock_sync_interval_s`, `ops.exchange_info_refresh_s`, `ops.unknown_block_cycles`, `ops.unmanaged_assets`, `execution.protective_stop_type`, `execution.stop_offset_bps`, `execution.take_profit_offset_bps`), şema |
| 11 | Dokunulmayacak | Risk kuralları |
| 12 | Çalışma yüzeyi | `traderd demo-check -config <f> -credentials <demo.json> -killswitch-credentials <demo2.json>` |
| 13 | Sözleşmeler | Emir durum geçiş tablosu; olay kind'ları (`intent_persisted`, `order_submitted`, `order_state`, `fill`, `protect_placed`, `protect_failed`, `unprotected_window`, `dust`, `reconcile_diff`, `exchange_info_changed`) |
| 14 | Config/doküman | Yukarıdaki alanlar; runbook |
| 15 | Testler | Sahte borsa (httptest + sahte WS API): 5XX/`-1007`/zaman aşımı → UNKNOWN → sorgu → doğru son durum; UNKNOWN girişte yeni deneme gönderilmez; IOC dolduktan sonra aynı ID'nin yeniden gönderimi yerelde engellenir; geç ortaya çıkan emrin sahiplenilmesi; kısmi dolum + baz varlıkta ücret → OCO net miktarı; OCO reddi → tek yeniden deneme → piyasa kapatma → başarısızlık → alarm ve blok; dust; çıkış sırası ve SELL reddinde OCO'nun geri konması; kullanıcı akışı kopması → yeniden abone + mutabakat; `balanceUpdate` fark sayılmaz; dolum ile OCO arasında `SIGKILL` → yeniden başlatmada E-10 OCO yerleştirir; ID regex ve 31 karakter (fuzz); piyasa hostu 429'u iptali engellemez |
| 16 | Doğrulama | Ortak komutlar |
| 17 | Build | `go build ./...` |
| 18 | Canlı doğrulama | `go test -tags live -run TestDemo ./internal/exchange/binance/ -args -credentials ~/.config/trader/demo.json` ve `go run ./cmd/traderd demo-check ...` (iki kez, farklı günlerde); Demo bakım takvimi kontrol edilir |
| 19 | Riskler | Çift emir → E-1..E-4; korumasız pozisyon → E-9..E-11; Demo ile canlı farkı → canlı tatbikat W7 |
| 20 | Geri alma | Demo dışında emir yok |
| 21 | Çıkış kriteri | Testler + inceleme temiz; Demo senaryosu iki kez geçti; K-1 ve K-8 ölçüm sonucu `docs/exchanges/binance.md`'de |
| 22 | Sonraki adım | W2c |

### W2c — Paper simülatörü, orkestratör, kill switch, alarm, gözlem, dağıtım

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | LLM'siz sistemin canlı veriyle paper modunda uçtan uca, gözlemlenebilir ve güvenli çalışması; kill switch'in tüm tetikleyicileriyle çalışması |
| 2 | Kaynak | Plan: kill switch bölümü (tetikleyiciler, iki iptal sınıfı, elle tetikleme, yeniden başlatma, tatbikat); D-6; I-4 |
| 3 | Kapsam | (a) `internal/exchange/sim`: `execution` ile aynı arayüz. LIMIT: yalnız limit fiyatının **ötesinde** gerçekleşen aggTrade ile, işlem miktarıyla sınırlı dolum (kuyruk önceliği hiç verilmez). MARKET ve tetiklenmiş `STOP_LOSS`: defterin karşı tarafından seviye seviye. Stop tetiği: stop fiyatında ya da ötesinde aggTrade. OCO: bir bacak dolunca diğeri iptal. Ücret: hesabın ölçülen oranları; tüm filtreler borsadaki gibi. (b) `internal/orchestrator`: BOOT → PREFLIGHT → RECONCILE → RUNNING ↔ DEFENSIVE → HALTED; yeni giriş yalnız RUNNING'de; her adımda zaman aşımı. (c) `cmd/killswitch`: ayrı süreç, ayrı kullanıcı, ayrı anahtar; kendi preflight'ı (I-7, saat); kendi REST istemcisi (filtre yuvarlamalı). Tetikleyiciler: (1) heartbeat kaybı (`ops.heartbeat_timeout_s`) → `e` emirleri iptal, `p` korunur, giriş kilidi; (2) R-1: kill switch özsermayeyi **kendisi** hesaplar (hesap bakiyeleri + `bookTicker` orta fiyatları, `ops.killswitch_equity_interval_s`), zirveyi kendi dosyasında tutar; aşımda `risk.on_max_dd`: `close_all` (E-11) ya da `hold_stops` (eksik `p` stoplarını depodaki fiyatlarla yeniden yerleştirir); (3) traderd'den kill isteği (R-19 tırmanması); (4) elle: `killswitch trigger {cancel_entries,close_all}` CLI ve Telegram komutu (U-7). Kill switch'in hedefi açık bir ayardır: `killswitch.target` ∈ {`sim`, `demo`, `live`}; `sim` → traderd'nin sim kontrol soketi, `demo` → demo-api.binance.com, `live` → api.binance.com. `traderd demo-check` ve `traderd drill` kill switch'i kendi hedefleriyle (`demo`, `live`) ayrı bir örnek olarak başlatır ve hedef uyuşmazlığında çalışmaz. Kill switch, `hold_stops` için gereken stop fiyatlarını veritabanından değil heartbeat mesajından alır ve kendi dosyasında tutar (veritabanına erişimi yoktur). Olaylarını kendi dosyasına yazar, traderd içe alır (ADR-002). Unix soketleri `0660`, ortak grup `trader`. Yeniden başlatma yalnız `traderd approve` + U-19 imzası. (d) Deterministik R-16: Binance Announcements WebSocket (`com_announcement_en`, `catalogName` = "Delisting") başlığında evrendeki ya da açık pozisyondaki bir sembolün `XXXUSDT` biçimi ya da baz varlığı **tam kelime** olarak geçerse (ör. `BTC`, `WBTC` içinde eşleşmez) `DelistBlocked`: yeni giriş yok ve açık sistem pozisyonu bir sonraki sinyal döngüsünde E-11 ile kapatılır (son tarihten önce çıkış; tarih metinden ayrıştırılmaz, beklenmez). Ayrıca `exchangeInfo` durum değişikliği (W2b). (e) R-21: U-18 kaynağından USDT/USD; sapma `risk.fx_shock_pct` → DEFENSIVE. (f) R-23: borsa olayı bayrağı = elle `traderd venue-halt` + Announcements'ta tanımlı kategoriler (U-8). (g) `internal/alert`: Telegram; hız sınırı, tekilleştirme, sır yok. (h) Gözlem: `log/slog` JSON → journald; `127.0.0.1`'de bağımlılıksız Prometheus metin `/metrics`, `/healthz`. (i) Kapanış: SIGTERM → giriş durur → açık `e` emirleri iptal → `p` korunur → yazmalar → çıkış (`ops.shutdown_timeout_s`). (j) `deploy/systemd/{traderd,killswitch}.service`: ayrı kullanıcılar, `NoNewPrivileges`, `ProtectSystem=strict`, `LoadCredentialEncrypted`, `MODERNC_SQLITE_OFD_LOCK=1`; AppArmor profilleri `deploy/apparmor/*` |
| 4 | Kapsam dışı | LLM, gerçek emir |
| 5 | Ön koşul | W2b; U-4, U-7, U-8 (yalnız Binance duyuruları), U-17 (çıkıştan önce), U-18, U-19 |
| 6 | Varsayımlar | Küçük emirlerde piyasa etkisi ihmal edilebilir (assumption, plan v1; W7'de canlı defterle sınanır). Bir anahtarın aynı hesaptaki başka anahtarın emrini iptal edebilmesi belgelenmemiş (K-8): W2b Demo ölçümü olumsuzsa kill switch traderd'nin anahtarını salt-iptal amaçlı kullanır ve bu yeni bir karar olarak sunulur |
| 7 | Blocker | U-4, U-7, U-8, U-18, U-19; çıkış için U-17 |
| 8 | İncelenecek | `internal/{execution,reconcile,risk,store}` |
| 9 | Eklenecek | `internal/{orchestrator,alert,obs,announce,depeg}/*`, `internal/exchange/sim/*`, `cmd/killswitch/*`, `deploy/{systemd,apparmor}/*`, `docs/runbook/{kill-switch,deploy}.md` |
| 10 | Değişecek | `cmd/traderd` (`run`, `approve`, `venue-halt`), config (`alert.*`, `ops.metrics_addr`, `ops.shutdown_timeout_s`, `ops.killswitch_equity_interval_s`, `killswitch.target`, `depeg.*`, `announce.*`), şema |
| 11 | Dokunulmayacak | Risk kuralları, emir durum makinesi sözleşmesi |
| 12 | Çalışma yüzeyi | `traderd run -config <f>`, `killswitch -config <f>`, `killswitch trigger close_all` |
| 13 | Sözleşmeler | Heartbeat ve kill isteği protokolü (sıra no, zaman, durum, özsermaye özeti, açık sistem pozisyonları ve stop/kar al fiyatları); sim kontrol soketi; kill olay formatı |
| 14 | Config/doküman | Yukarıdaki alanlar; runbook'lar |
| 15 | Testler | Simülatör: limit fiyatına eşit işlem dolum üretmez, ötesindeki işlem miktarıyla sınırlı; stop tetiği ve MARKET dolumu; OCO karşı bacak iptali; kill: heartbeat kaybı → yalnız `e` iptal; R-1 → `close_all` ve `hold_stops` (eksik stop yeniden yerleşir); elle tetik (CLI, Telegram, yetkisiz chat id reddi); traderd kill isteği; önek tanınmayan emre dokunulmaz; `killswitch.target` uyuşmazlığında başlamama; heartbeat'teki stop fiyatlarıyla `hold_stops` (veritabanı olmadan); SIGTERM sırası; metrik ucu yalnız loopback; alarmda sır yok; Announcements çift ayrıştırma ve PING; delist eşleşmesinde tam kelime (`WBTC` ≠ `BTC`) ve pozisyon kapatma |
| 16 | Doğrulama | Ortak komutlar |
| 17 | Build | `CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/...` |
| 18 | Canlı doğrulama | VPS'te 7 gün `traderd run` (paper) + kill switch; elle kill tatbikatı paper'da (sim soketi) ve Demo'da (gerçek uçlar) |
| 19 | Riskler | Simülatör iyimserliği → muhafazakâr kurallar; kill switch'in özsermaye hesabı ile traderd'ninkinin ayrışması → fark alarmı |
| 20 | Geri alma | Paper; `systemctl stop` |
| 21 | Çıkış kriteri | 7 günde 0 mutabakat farkı, 0 panik, paper ve Demo kill tatbikatları geçti, inceleme temiz; U-17 eşikleri kayıtlı |
| 22 | Sonraki adım | W4 (W3 paralel) |

### W3 — Araştırma: veri, replay/backtest, istatistik, baseline'lar

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Kural tabanlı sinyalleri uzun geçmişte, maliyetli ve aşırı uyum kontrollü seçmek; kapılar için baseline üretmek |
| 2 | Kaynak | data.binance.vision README: günlük/aylık zip + `.CHECKSUM`, spot zaman damgaları **2025-01-01'den itibaren mikrosaniye**, öncesi milisaniye (fact); López de Prado (CPCV, DSR, PBO, MinTRL) |
| 3 | Kapsam | (U-15, U-20, U-21 onayına bağlı) (a) `cmd/datasync`: kline ve aggTrade arşivini indirir, SHA-256 doğrular, ms/µs normalize eder, `research/data/` altına Parquet değil sıkıştırılmış CSV yazar (bağımlılıksız); `updates/` yeniden indirme. (b) `cmd/backtest`: `internal/features` ve `internal/signal` kodunu **aynen** kullanan olay tabanlı replay; aday kurallar `internal/signal` kayıt defterine eklenir, orkestratöre bağlanmaz; parametreler `internal/params` (W2a) ile `policies/` altından; maliyet modeli = ölçülen ücret + yarım spread + slipaj parametresi; çıktı: işlem listesi ve günlük getiri CSV'si. (c) `research/` (Python, `uv` ile kilitli bağımlılıklar): CPCV (purge + embargo), DSR, PBO, bootstrap güven aralığı, deneme kayıt defteri (her deneme bir satır, silinemez), baseline'lar: BTC/USDT al-tut, eşit ağırlıklı sepet, basit momentum, eşleşik devirli rastgele strateji. (d) Tarihsel evren: o tarihte bilinen bilgiyle yeniden kurulum; delist edilmiş sembollerin arşivde tutulup tutulmadığı **unknown** → mevcut olmayan semboller raporlarda survivorship sınırlaması olarak yazılır |
| 4 | Kapsam dışı | LLM'li backtest (H-1: kanıt sayılmaz) |
| 5 | Ön koşul | W2a; U-15, U-20, U-21 |
| 6 | Varsayımlar | Yok. Vekil veri sorunu D-17 ile ortadan kalktı (işlem yapılacak USDT pariteleri ile arşiv aynı) |
| 7 | Blocker | U-15, U-20, U-21 |
| 8 | İncelenecek | `internal/features`, `internal/signal`, `internal/sizing` |
| 9 | Eklenecek | `cmd/datasync/*`, `cmd/backtest/*`, `research/{pyproject.toml,uv.lock,src/trader_research/{cv,metrics,registry,baselines,report}/*,tests/*}`, `docs/research/methodology.md` |
| 10 | Değişecek | CI: `research` için `uv run pytest` işi |
| 11 | Dokunulmayacak | Orkestratöre bağlı kuralların davranışı (aday kurallar yalnız kayıt defterine eklenir) |
| 12 | Çalışma yüzeyi | `datasync -from 2023-01 -symbols ...`; `backtest -params policies/v0000/params.yaml -out runs/<id>`; `uv run trader-research cpcv runs/<id>` |
| 13 | Sözleşmeler | Backtest çıktı CSV şeması (Go yazar, Python okur); deneme kayıt defteri şeması |
| 14 | Config/doküman | Metodoloji dokümanı (DSR ≥ 0,95, PBO ≤ 0,2 eşikleri önceden kayıtlı; tasarım seçimi) |
| 15 | Testler | Go: replay determinizmi (aynı girdi → aynı çıktı hash'i), ms/µs normalizasyonu, checksum hatası; Python: bilinen çözümlü DSR/PBO örnekleri, purge/embargo sızıntı testi |
| 16 | Doğrulama | Ortak komutlar + `cd research && uv run pytest` |
| 17 | Build | `go build ./cmd/...`; `uv sync --locked` |
| 18 | Canlı doğrulama | `data.binance.vision` erişimi (ÖLÇÜM-2'de 200); indirme VPS'te ya da kullanıcının makinesinde |
| 19 | Riskler | Kural seçiminde aşırı uyum → her deneme kayıtlı ve DSR'ye sayılır |
| 20 | Geri alma | Araştırma; canlıya etkisi yok |
| 21 | Çıkış kriteri | Seçilen kural seti için DSR/PBO raporu ve baseline'lar aynı maliyet modeliyle; inceleme temiz |
| 22 | Sonraki adım | Seçilen kurallar W5'te `internal/signal`'a taşınır |

### W4 — LLM katmanı, A1 ve A2 (yalnız shadow)

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | LLM altyapısını karar yoluna dokunmadan kurmak ve maliyet/hata oranını ölçmek |
| 2 | Kaynak | `research/E_binance_impl_facts.md` §I: `github.com/anthropics/anthropic-sdk-go/v2` v2.11.0 (Go 1.25+); API sürümü 2026-10-21'de `temperature`/`top_p`/`top_k` 400 döner; structured outputs `output_config.format` beta başlığı gerektirmez, min/max kısıtlarını zorlamaz; prompt cache minimum 512 token; Batch %50. GH Announcements WS (`com_announcement_en`, `data` alanı iki kez ayrıştırılır, istemci 30 sn'de bir PING) |
| 3 | Kapsam | (a) `internal/llm`: SDK sarmalayıcı (context, zaman aşımı, yeniden deneme yalnız 429/529/bağlantı hatasında ve sınırlı); structured outputs; şema doğrulama (Go tarafında ikinci kez) + uzunluk + `evidence_refs` gerçekten girdide mi kontrolü; `stop_reason` refusal / max_tokens / şema hatası → abstain; her çağrı olay günlüğüne (girdi hash'i + tam girdi, çıktı, usage, gecikme, politika sürümü). (b) Maliyet valisi: usage'dan USD hesabı (fiyat tablosu politika paketinde, kaynaklı), aylık tavan = `llm.cost_cap_pct_of_capital` × özsermaye (canlı) ya da `llm.rnd_monthly_cap_usd` (paper); öncelik A1 > A4 > A3 > A2 > A5; tavan aşımı → I-4 güvenli mod. (c) `internal/news`: W2c'deki Announcements istemcisini yeniden kullanır + U-8 beyaz listesindeki diğer kaynaklar; Unicode NFKC, gizli HTML/zero-width silme (`golang.org/x/text`, U-8); haber kaynaklı bir giriş için en az iki bağımsız kaynak kuralı kodda (kaynak kimliği farklı ve beyaz listede). (d) `internal/policy`: ADR-003 paket yükleyici (`internal/params`'ı genişletir), `SHA256SUMS` doğrulama, işaretçi. (e) A1 ve A2 çağrı mantığı; çıktılar yalnız günlüğe (shadow). (f) Regresyon suite: 20 dondurulmuş vaka, pass^3 |
| 4 | Kapsam dışı | A3, A4; LLM'in karara etkisi |
| 5 | Ön koşul | W2c; U-8, U-9 |
| 6 | Varsayımlar | Yok |
| 7 | Blocker | U-8, U-9 |
| 8 | İncelenecek | `internal/orchestrator`, `internal/features`, `internal/params`, ADR-003 |
| 9 | Eklenecek | `internal/{llm,news,policy}/*`, `internal/agents/{events,regime}/*`, `policies/v0001/*`, `testdata/evals/*` |
| 10 | Değişecek | `internal/orchestrator`, `go.mod` (SDK v2.11.0), config, şema |
| 11 | Dokunulmayacak | `internal/risk`, `internal/execution` |
| 12 | Çalışma yüzeyi | Paper + shadow; `traderd eval -suite testdata/evals -k 3` |
| 13 | Sözleşmeler | A1, A2 JSON şemaları (`additionalProperties: false`, tüm alanlar zorunlu) |
| 14 | Config/doküman | `llm.*`, `agents.events`, `agents.regime`, `news.*`; `docs/agents.md` |
| 15 | Testler | İki bağımsız kaynak kuralı (aynı kaynaktan iki haber yetmez); httptest ile: refusal, max_tokens, 429, 529, 400 (sampling parametresi gönderilmediğinin testi), şema ihlali, uydurma referans, harcama limiti; homoglyph/gizli HTML; maliyet hesabı golden; Announcements çift ayrıştırma ve PING zamanlaması |
| 16 | Doğrulama | Ortak komutlar + regresyon suite pass^3 |
| 17 | Build | `go build ./...` |
| 18 | Canlı doğrulama | VPS'te 7 gün shadow; ölçülen maliyet ve abstain oranı raporu |
| 19 | Riskler | Prompt injection → A1 karantinası; maliyet aşımı → vali + workspace limiti |
| 20 | Geri alma | `agents.*.enabled: false` |
| 21 | Çıkış kriteri | 0 yakalanmamış şema hatası; maliyet tavan içinde; inceleme temiz |
| 22 | Sonraki adım | W5 |

### W5 — A3 ve A4 karar yolunda (paper), kural taşıma, rol katkısı

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Swing niyetini LLM önersin, risk motoru karar versin; her rolün katkısı ölçülsün |
| 2 | Kaynak | Plan: ajan spesifikasyonları, H-5 |
| 3 | Kapsam | A3 (yalnız long/flat; sayı üretmez; Sonnet 5.5'e yükseltme yalnız deterministik koşulda — A2 rejimi `chaotic` ya da A3/A4 uyuşmazlığı — ve günlük `agents.thesis.max_escalations_per_day` tavanıyla), A4 (yalnız azaltır/veto; büyütme girişimi kodda reddedilir), kapalı-rol varsayılanları (plan tablosu), A2 izninin intraday sinyalini yalnız daraltması, W3'te seçilen kuralların `internal/signal`'a taşınması (backtest ile aynı kod), rol katkısı: rolü kapalı gölge kopya ile aynı veride karşılaştırma, karar başına maliyet metriği. Gözlem: `gen_ai.*` öznitelik adları log ve metriklerde; OTel exporter eklenmez (U-21) |
| 4 | Kapsam dışı | Canlı emir |
| 5 | Ön koşul | W4, W3 |
| 6 | Varsayımlar | Yok |
| 7 | Blocker | Yok |
| 8 | İncelenecek | `internal/{risk,sizing,agents,signal}` |
| 9 | Eklenecek | `internal/agents/{thesis,critic}/*`, `policies/v0002/*` |
| 10 | Değişecek | `internal/orchestrator`, `internal/sizing` (A4 çarpanı ≤ 1), `internal/signal` |
| 11 | Dokunulmayacak | Risk kuralları |
| 12 | Çalışma yüzeyi | Paper |
| 13 | Sözleşmeler | A3, A4 şemaları; A4 çarpanı ∈ {0, 0.5, 1} enum |
| 14 | Config/doküman | `agents.thesis`, `agents.critic`; `docs/agents.md` |
| 15 | Testler | Yükseltme tavanı aşılınca Haiku sonucu kullanılır; A4 büyütme girişimi reddi; abstain → yeni risk yok; LLM kesintisi → güvenli mod; kural taşıma sonrası backtest çıktısının W3 raporuyla birebir aynı olması |
| 16 | Doğrulama | Ortak komutlar + regresyon suite |
| 17 | Build | `go build ./...` |
| 18 | Canlı doğrulama | VPS'te 14 gün paper |
| 19 | Riskler | Aşırı işlem → R-11, R-17, R-18 |
| 20 | Geri alma | Politika işaretçisi v0001 |
| 21 | Çıkış kriteri | Her emir için uçtan uca iz; rol katkı raporu; inceleme temiz |
| 22 | Sonraki adım | W6 |

### W6 — Öğrenme hattı

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | L-1..L-4'ün terfi hattıyla, onaysız hiçbir değişiklik canlıya çıkmadan çalışması (I-5) |
| 2 | Kaynak | Plan: öğrenme döngüsü; Batch API (`custom_id` `^[a-zA-Z0-9_-]{1,64}$`, sonuçlar 29 gün; fact E §I) |
| 3 | Kapsam | A5 gecelik batch (Go, SDK `Messages.Batches`; U-15), ders karantinası (`learning.min_supporting_trades`, `learning.max_param_delta_pct`), few-shot kütüphanesi, hafıza snapshot ve geri alma, shadow aday çalıştırıcı, `cmd/replay` (kayıtlı LLM çıktılarıyla deterministik yeniden oynatma), karşılaştırma raporu, `traderd policy {promote,rollback}` (imzalı onayla) |
| 4 | Kapsam dışı | Otomatik terfi |
| 5 | Ön koşul | W5; U-15 |
| 6 | Varsayımlar | Yok |
| 7 | Blocker | U-15 (değerler D-16'da) |
| 8 | İncelenecek | `internal/{policy,store}`, `research/` kayıt defteri |
| 9 | Eklenecek | `internal/{memory,reflection}/*`, `cmd/replay/*`, `docs/runbook/promotion.md` |
| 10 | Değişecek | `internal/agents/thesis` (onaylı ders/örnek okuma) |
| 11 | Dokunulmayacak | Risk motoru |
| 12 | Çalışma yüzeyi | systemd timer; CLI |
| 13 | Sözleşmeler | A5 şeması; hafıza kaydı (kaynak, hash, sürüm) |
| 14 | Config/doküman | `learning.*` |
| 15 | Testler | Ham dış metnin hafızaya yazılamaması; eşik altı dersin karantinada kalması; snapshot geri alma; replay determinizmi; onaysız terfinin reddi |
| 16 | Doğrulama | Ortak komutlar + `uv run pytest` |
| 17 | Build | `go build ./...` |
| 18 | Canlı doğrulama | Bir tam terfi + geri alma tatbikatı (paper) |
| 19 | Riskler | Hafıza zehirlenmesi; az örnekten sahte ders |
| 20 | Geri alma | Snapshot ve işaretçi |
| 21 | Çıkış kriteri | Tatbikat başarılı, tüm adımlar günlükte; inceleme temiz |
| 22 | Sonraki adım | W7 |

### W7 — Paper soak, sertleştirme ve G1

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Canlı paraya operasyonel hazırlığı kanıtlamak |
| 2 | Kaynak | D-14, D-16 (`gates.paper_min_days` = 60) |
| 3 | Kapsam | 60 gün kesintisiz paper; VPS sertleştirme (otomatik güvenlik güncellemesi, yalnız anahtarla SSH, ufw, ayrı servis kullanıcıları); sırların `systemd-creds` ile şifrelenmesi; günlük şifreli yedek (`age`, U-20) VPS dışına — veritabanı ve `.anchor` çapası (ayrıca her çapa güncellemesinde uç hash'i VPS dışı bir hedefe kopyalanır; yerel kötü niyetli yazıcıya karşı tek savunma budur, bkz. `docs/runbook/storage.md` tehdit modeli), kill switch olay dosyası, politika paketleri, hafıza snapshot'ları, config — + geri yükleme tatbikatı (sonra her çeyrek); VPS dışı heartbeat izleyici (U-10); `traderd drill` alt komutu: canlı anahtarla (U-11) en küçük geçerli giriş → OCO → `killswitch.target: live` ile başlatılan kill switch örneğinden `close_all`; sabit tutar tavanı (U-22), etkileşimli onay, yalnız bu komut `mode: paper` dışında emir gönderebilir; simülatör maliyet modelinin canlı defter ölçümüyle karşılaştırılması; G1 raporu (`research` + Go) |
| 4 | Kapsam dışı | Tatbikat dışında gerçek emir |
| 5 | Ön koşul | W6; U-10, U-11, U-22 (U-17 W2c'de kayıtlı) |
| 6 | Varsayımlar | Yok |
| 7 | Blocker | U-10, U-11, U-22 |
| 8 | İncelenecek | Tüm `internal/*`, `deploy/*` |
| 9 | Eklenecek | `deploy/backup/*`, `cmd/traderd/drill.go`, `docs/runbook/{incident,restore,gate-g1}.md`, G1 rapor üreticisi |
| 10 | Değişecek | Bulunan hatalar |
| 11 | Dokunulmayacak | Kapı eşikleri (rapor sonrası değiştirilemez) |
| 12 | Çalışma yüzeyi | VPS üretim düzeni |
| 13 | Sözleşmeler | G1 rapor formatı |
| 14 | Config/doküman | `gates.*` |
| 15 | Testler | Kill tatbikatı; VPS yeniden başlatmada güvenli dönüş; ağ kesintisinde emir durumu; yedekten tam geri yükleme ve zincir doğrulama |
| 16 | Doğrulama | G1 kriterleri (plan tablosu; U-17'de kayıtlı eşikler; baseline'lar BTC/USDT al-tut, momentum, eşit ağırlıklı sepet) |
| 17 | Build | Sürüm etiketli, `-trimpath` binary; SHA-256 |
| 18 | Canlı doğrulama | Kill tatbikatı canlı anahtarla, kullanıcı gözetiminde |
| 19 | Riskler | Kısa paper'da güven aralığı geniş → G1 haftalarca kapalı kalabilir (beklenen davranış) |
| 20 | Geri alma | `mode: paper` |
| 21 | Çıkış kriteri | G1 raporu geçti + imzalı onay |
| 22 | Sonraki adım | W8 |

### W8 — Küçük sermaye canlı (200 USD) ve G2

| # | Alan | İçerik |
| --- | --- | --- |
| 1 | Amaç | Gerçek parayla istatistiksel kanıt toplamak |
| 2 | Kaynak | D-14, D-16 (`gates.g2_min_days` = 180, `capital.live_small_max_usd` = 200) |
| 3 | Kapsam | Config'te `mode: live_small` bu dalgada etkinleşir (şu an reddediliyor, ADR-004); haftalık performans, maliyet ve rol katkı raporu; aylık vergi defteri dışa aktarımı; G2 öncesi kill switch canlı tatbikatı (`traderd drill`); G2 raporu |
| 4 | Kapsam dışı | Sermaye artırımı |
| 5 | Ön koşul | G1 onayı; U-12, U-13 |
| 6 | Varsayımlar | Yok |
| 7 | Blocker | U-12 |
| 8–11 | Dosyalar | Yalnız hata düzeltmeleri ve raporlar |
| 12 | Çalışma yüzeyi | Canlı binance.com |
| 13 | Sözleşmeler | G2 rapor formatı |
| 14 | Config/doküman | `mode: live_small` |
| 15 | Testler | Her değişiklik W2–W6 suite'lerini geçer |
| 16 | Doğrulama | G2 kriterleri |
| 17 | Build | Sürüm etiketli binary |
| 18 | Canlı doğrulama | Sürekli |
| 19 | Riskler | Gerçek kayıp; borsa olayı; USDT depeg; Türkiye'den erişim engeli (D-17 kabul edilen risk) |
| 20 | Geri alma | Kill switch; `mode: paper` |
| 21 | Çıkış kriteri | G2 raporu geçti + imzalı onay |
| 22 | Sonraki adım | W10 |

### W10 — Tam sermaye işletim

`mode: live_full`. Anahtar rotasyonu, çeyreklik limit gözden geçirmesi, model geçişi (terfi hattıyla; Haiku 5.5 en erken emeklilik 7 Ekim 2027), vergi defteri dışa aktarımı, çeyreklik geri yükleme tatbikatı, SPK lisans durumu ve kripto vergi teklifinin izlenmesi, olay sonrası postmortem. Limit değişikliği ve kill sonrası yeniden başlatma her zaman insan onayındadır.

### W9 — BtcTurk (ertelendi)

ADR-005 tek borsa kararı verdi. W9 başlamadan önce yeni bir ADR (gerekçe, ücret tablosu doğrulaması, OCO yokluğunda istemci taraflı stop riski, R-23 değerleri) gerekir.

## 6. Açık bilinmeyenler (unknown) ve kapanış yeri

| # | Bilinmeyen | Kapandığı yer |
| --- | --- | --- |
| K-1 | REST client order ID biçim kuralı ve iptal sonrası yeniden kullanım | W2b Demo ölçümü (tasarım ikisine de dayanmaz) |
| K-2 | Delist edilmiş sembollerin data.binance.vision'da tutulması | W3 (yoksa survivorship sınırlaması raporlanır) |
| K-3 | Varlık sınıfı (stablecoin, bStock, emtia) için resmi alan | Yok (fact: belgelenmemiş) → izin listesi (U-5) |
| K-4 | Announcements WS'nin Ed25519 kabul edip etmediği | W4 canlı deneme; yoksa ayrı yalnız-okuma HMAC anahtarı (U-8) |
| K-5 | Binance'in kısıtladığı ülke/bölge listesi | U-4: VPS bölgesinde `venuecheck` ile ölçülür |
| K-6 | `quoteNotionalLimit` etkinleşme tarihi (Binance: "karar verilmedi") | Tasarım kullanmaz |
| K-7 | Türkiye'den binance.com erişiminin sürekliliği | İzleme; W8 riski |
| K-8 | Bir API anahtarının aynı hesaptaki başka anahtarın emrini iptal edebilmesi | W2b `demo-check` (iki Demo anahtarı) |
| K-9 | `data-api.binance.vision` ağırlığının `api.binance.com` ile paylaşılıp paylaşılmadığı (doküman yalnız "IP bazlı" diyor) | W1b observe: iki hostun `X-MBX-USED-WEIGHT-1M` başlıkları karşılaştırılır |
| K-10 | `/sapi` uçlarının Demo Mode'da çalışması | W1c/W2b |
| K-12 | Ping'lere cevap verip veri göndermeyen (donmuş) bir akış: bağlantı korunur, semboller R-9 ile stale olur; ikinci bir veri-sessizliği zaman aşımı gerekip gerekmediği | W1b 24 sa observe (stale süreleri) |
| K-11 | OTOCO'da çalışan emir için FOK desteği (dolumdan sonra boşluksuz koruma alternatifi) | W2b Demo; olumluysa E-9'a alternatif olarak ayrı karar |
