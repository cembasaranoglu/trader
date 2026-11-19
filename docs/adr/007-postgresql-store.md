# ADR-007: Depolama PostgreSQL 16 (yönetilen, AWS Lightsail Tokyo)

- Durum: Kabul edildi (2026-11-19, kullanıcı kararları U-23, U-4, U-24)
- Yerini aldığı: ADR-002 (SQLite), ADR-006 (SQLite sürücüsü)
- Kaynak: proje dokümanı `research/F_database_options.md`

## Bağlam

Kullanıcı SQLite'ı reddetti: veritabanı bir bulut sağlayıcının yönetilen servisi olmalı, yerelde Docker ile çalışmalı, sistem ölçeklenebilmeli ve yarış koşulu olmamalı. Emir yolu (E-2) her niyeti göndermeden önce commit ettiği için veritabanı gecikmesi emir yolundadır.

## Karar

1. **Motor:** PostgreSQL 16 (Lightsail yönetilen veritabanının sunduğu en yeni ana sürüm). Kod sürüm 16 ve üstünü kabul eder (`server_version_num ≥ 160000`); CI ve yerel Docker 16 kullanır.
2. **Barındırma:** AWS Tokyo (ap-northeast-1), Lightsail yönetilen PostgreSQL. VPS de aynı bölgede (Lightsail). Bölge, VPS'ten `venuecheck` geçince kesinleşir.
3. **Sürücü:** `github.com/jackc/pgx/v5` v5.13.0 (`pgxpool`).
4. **Rol ayrımı:** ayrı bir sahip rol (`trader_owner`; Lightsail ana kullanıcısının kurulumda oluşturduğu, rol oluşturamayan) migration'ları çalıştırır (`traderd store migrate`); bağlantı dizesi yalnız `migrate` servisine bağlanır, ana kullanıcının parolası VPS'te saklanmaz. Uygulama rolü kayıt tablolarında yalnız SELECT/INSERT, `intents`/`orders`'da SELECT/INSERT/UPDATE, kira fonksiyonlarında EXECUTE yetkisi alır; geçici tablo oluşturamaz. `traderd` süper kullanıcı, rol oluşturabilen, tablo sahibi/sahibin üyesi ya da fazladan yetkili bir rolle açılmayı reddeder.
4a. **`search_path` sertleştirmesi:** her fonksiyon `search_path = pg_catalog, public, pg_temp` sabitler ve tabloları/fonksiyonları şemasıyla adlandırır; fonksiyonlar PUBLIC'e kapalıdır. (Bağımsız inceleme, bunlar yokken uygulama rolünün geçici bir tabloyla SECURITY DEFINER fonksiyonları üzerinden sahip rol olarak SQL çalıştırabildiğini kanıtladı; üç saldırı regresyon testidir.)
5. **Bütünlük veritabanında:** `events` ekleme tetikleyicisi `seq = son+1`, `prev_hash = son hash`, hash'in içerikle tutmasını (Go ile aynı fonksiyon) ve payload sözleşmesini (JSON nesnesi, tam sayı `"v"` ∈ [1, 10^18)) kontrol eder. Kayıt tabloları UPDATE/DELETE/TRUNCATE'i reddeder. Doğrulama migration sağlama toplamlarını, her koruyucu tetikleyicinin varlığını/açıklığını ve `public` şemasındaki fonksiyon kümesinin (kaynak, SECURITY DEFINER, `search_path`) migration'larla aynılığını kontrol eder.
6. **Eşzamanlılık:** ekleme işlemleri işlem düzeyinde advisory lock ile sıralanır; birden çok süreç güvenle ekler. Kilidi atlayan bir istemcinin çakışan eklemesini birincil anahtar ve tetikleyici reddeder.
7. **Tek yürütücü + fencing (U-24):** emir veren rol (`executor`) oturum düzeyinde advisory lock ile tek sürece verilir; her yeni sahibin `epoch`'u büyüktür. `intents`/`orders` yazmaları epoch taşır, tetikleyici eski epoch'u reddeder (`FOR SHARE` ile epoch artışıyla sıralanır). Kira bağlantısında sunucu tarafı TCP keepalive (10 sn + 3 × 5 sn) ve `tcp_user_timeout` (30 sn) ayarlanır; kopan bir sahibin oturumu ve kilidi yaklaşık 25–30 sn içinde düşer (bu sürede başka süreç kirayı alamaz; güvenlik değil süreklilik etkisi). İşçi süreçler (piyasa verisi, LLM, backtest) kira almadan ölçeklenir.
8. **Çapa:** zincir ucu yerel bir dosyada tutulur (VPS). Eski bir anlık görüntüden geri yükleme (`ErrAnchorAhead`) ve geçmişin yeniden yazılması (`ErrChainBroken`) yakalanır. Çapası olmayan bir hostta açılış reddedilir; `traderd store anchor-init` bilinçli bir operatör adımıdır.
9. **Yedek:** sağlayıcının otomatik yedekleri + `traderd store export` (çevrimdışı doğrulanabilir JSONL, bitiş satırı ve SHA-256). SQLite'taki `VACUUM INTO` yedeği kalktı.
10. **Bağlantı dizesi:** dosyada (chmod 600, süreç kullanıcısına ait). Yerel olmayan bir host için (çok hostlu dizelerde her host için) `sslmode=verify-full` ve açık bir CA (`sslrootcert`) zorunlu; düz metne geri düşme reddedilir. Ortam değişkeni (`TRADER_STORE_DSN`) yalnız tüm hostları yerel (Docker servisi, loopback, soket) olan bir dize için kabul edilir.

## Sonuçlar

- Veritabanı emir yolu için zorunlu bir bağımlılıktır: ulaşılamazsa yeni niyet yazılamaz, yeni emir verilmez (fail-closed). Kill switch veritabanına bağlı olmamalıdır (W2c'de ayrı olay dosyası).
- Aylık maliyet: Lightsail yönetilen veritabanı en küçük paket ~15 $ (araştırma tarihindeki liste fiyatı). Altyapı maliyeti kapı hesabına girer (`gates.infra_monthly_usd`, karar 2026-11-19).
- Tehdit modeli: uygulama hataları, uygulama kimlik bilgisinin kötüye kullanımı, kazalar ve bozulma yakalanır. Sahip rolün kimlik bilgisine sahip biri tetikleyicileri kaldırabilir; buna karşı savunma VPS dışı dışa aktarma ve çapa kopyalarıdır (W7).
- TLS (fact, Lightsail dokümanı): Lightsail sertifikası veritabanı uç noktasını CN olarak taşır; indirilebilen RDS etiketli CA paketleri Lightsail için de geçerlidir; `sslmode=verify-full` ile bağlanılır.
- Yedek (fact, Lightsail dokümanı): otomatik yedek ve son 7 gün içinde 5 dakikalık adımlarla yeni bir veritabanına geri yükleme; 7 günden uzun saklama için elle snapshot.
- Varsayım (kurulumda doğrulanacak): Lightsail ana kullanıcısı rol oluşturabilir (`CREATE ROLE trader_app`).
