# Runbook: depolama (PostgreSQL olay günlüğü)

Olay günlüğü ve alan tabloları PostgreSQL 16'dadır (ADR-007). Üretimde AWS Lightsail Tokyo'daki yönetilen veritabanı, yerelde Docker Compose'daki `postgres:16-alpine` kullanılır. VPS'teki `/var/lib/trader/trader.anchor` çapa dosyasıdır.

## Roller

| Rol | Kim kullanır | Yetki |
| --- | --- | --- |
| Sahip (`trader_owner`; Lightsail ana kullanıcısının oluşturduğu, rol oluşturamayan, yalnız bu veritabanının sahibi olan rol; yerel Docker'da süper kullanıcı) | yalnız `traderd store migrate`; bağlantı dizesi yalnız `migrate` servisine bağlanır | tabloları ve fonksiyonları oluşturur, uygulama rolüne yetki verir, `TEMPORARY` yetkisini PUBLIC'ten alır |
| Uygulama (`trader_app`) | `traderd` ve diğer alt komutlar | kayıt tablolarında SELECT/INSERT; `intents`/`orders`'da SELECT/INSERT/UPDATE; kira fonksiyonlarında EXECUTE |

`traderd` (ve `traderd preflight`, `traderd store verify`), süper kullanıcı, rol oluşturabilen, tablo sahibi ya da sahibin üyesi olan, geçici tablo oluşturabilen ya da uygulama rolünde olmayan bir yetkiye (UPDATE/DELETE/TRUNCATE, `leases`/`schema_migrations`'a INSERT) sahip bir rolle çalışmayı reddeder (`has more privileges`).

## Bütünlük modeli

- `events` yalnız eklenir. Her satır bir öncekine hash zinciriyle bağlıdır: `SHA-256(prev_hash ‖ be64 seq ‖ be64 ts_ns ‖ be32 len(kind) ‖ kind ‖ be32 len(payload) ‖ payload)`. İlk satırın `prev_hash`'i 32 sıfır bayttır.
- Veritabanı tetikleyicisi her eklemede `seq = son+1`, `prev_hash = son hash`, hash'in içerikle tuttuğunu ve payload sözleşmesini kontrol eder; uygulama kimlik bilgisiyle bile kırık bir zincir yazılamaz.
- `search_path` saldırılarına karşı: her fonksiyon `search_path = pg_catalog, public, pg_temp` sabitler ve nesneleri şemasıyla adlandırır, fonksiyonlar PUBLIC'e kapalıdır, uygulama rolü geçici tablo oluşturamaz. Bağımsız incelemede bulunan üç saldırı (geçici `leases` tablosuyla yetki yükseltme, geçici görünümle fencing'i aşma, geçici `events` tablosuyla zinciri bozma) regresyon testi olarak çalışır.
- Kayıt tabloları (`events`, `fills`, `balances_snapshot`, `approvals`, `tax_ledger`) UPDATE/DELETE/TRUNCATE'i reddeder; `intents`/`orders` DELETE/TRUNCATE'i reddeder.
- `intents`/`orders` yazmaları yürütücünün `epoch`'unu taşır; güncel olmayan epoch reddedilir (`fenced`). Tek yürütücü ve fencing: ADR-007 §7.
- Payload'lar tam sayı `"v"` (≥ 1) sürüm alanı taşıyan JSON nesneleridir; saklanan baytlar hash'lenen baytlardır.
- Doğrulama; uygulanmış migration'ların SHA-256'sını, her koruyucu tetikleyicinin var ve açık olduğunu ve doğru fonksiyonu çağırdığını, `public` şemasındaki fonksiyon kümesinin migration'larla birebir aynı olduğunu (kaynak, SECURITY DEFINER, sabit `search_path`) kontrol eder ve tüm zinciri tek tutarlı anlık görüntüde yeniden hash'ler.
- Çapa: her commit'ten sonra zincir ucu çapaya atomik yazılır. Veritabanı çapadan önce bitiyorsa (eski bir anlık görüntü geri yüklendi) ya da çapadaki hash tutmuyorsa (geçmiş yeniden yazıldı) açılış ve doğrulama başarısız olur. Aynı hostta birden çok süreç aynı çapa dosyasına yazarsa çapa kısa süre geriye gidebilir (her süreç yalnız kendi son yazdığını izler); bu yine geçerli bir önektir, yalnız tespitin keskinliği azalır.
- Dışa aktarma dosyası bir bitiş satırı (olay sayısı ve uç hash) taşır; sonu kesilmiş bir dosya doğrulamadan geçmez.

**Tehdit modeli.** Uygulama hataları, uygulama kimlik bilgisinin kötüye kullanımı, kazalar ve bozulma yakalanır. Sahip rolün kimlik bilgisine sahip biri tetikleyicileri kaldırabilir; buna karşı savunma VPS dışındaki dışa aktarma ve çapa kopyalarıdır (W7).

## Komutlar

```sh
traderd store migrate     -config <file>   # sahip rol: migration + yetkiler (tekrar çalıştırmak güvenli)
traderd store verify      -config <file>   # salt okuma: şema, korumalar, zincir, çapa
traderd store export      -config <file>   # export_dir/trader-events-<UTC>.jsonl (+ .sha256), 0600
traderd store verify      -export <file>   # dışa aktarmayı çevrimdışı doğrular (.sha256 varsa önce o)
traderd store anchor-init -config <file>   # çapası olmayan hostta çapayı oluşturur (aşağıya bakın)
```

Bağlantı dizeleri `store.dsn_file` ve `store.migrate_dsn_file` dosyalarından okunur (chmod 600, süreci çalıştıran kullanıcıya ait; konteynerde uid 65532). Yerel olmayan bir host için `sslmode=verify-full` ve `sslrootcert` zorunludur. Yalnız yerel veritabanı için ortam değişkeni kabul edilir: `TRADER_STORE_DSN`, `TRADER_STORE_MIGRATE_DSN`. Hata mesajları bağlantı dizesini ya da parolayı yazmaz.

## Yedek ve geri yükleme

- **Sağlayıcı yedeği:** Lightsail otomatik yedek alır; son 7 gün içinde 5 dakikalık adımlarla **yeni** bir veritabanına geri yüklenebilir (Lightsail konsolu → veritabanı → Snapshots & restore → Emergency restore). 7 günden uzun saklama için elle snapshot alın.
- **Dışa aktarma:** `traderd store export` tek tutarlı anlık görüntüyü yazarken zinciri de doğrular. Dosya ve `.sha256` 0600'dür; hata olursa yarım dosya kalmaz. VPS dışına şifreli taşıma W7'dedir.

Geri yükleme sırası:

1. Servisleri durdurun (`docker compose stop`).
2. Lightsail'de geri yüklemeyi yeni bir veritabanına yapın. Geri yüklenen veritabanı yeni bir uç nokta ve Lightsail'in oluşturduğu yeni bir ana parola alır; `trader_app` rolü yedeğin içindedir.
3. `/opt/trader/secrets/owner/db-owner.dsn` ve `/opt/trader/secrets/app/db-app.dsn` dosyalarını yeni uç noktayla güncelleyin. (Rollerin parolaları yedekle birlikte gelir; Lightsail yalnız ana kullanıcıya yeni parola verir.)
4. `docker compose run --rm verify` çalıştırın. Geri yükleme çapadan **eski** bir ana yapıldıysa `event log ends before the anchor` hatası beklenir: bu, kaybolan olaylar olduğunu söyler. Kayıp bilinçli olarak kabul edilmeden çapa silinmez. Kabul edilirse eski çapayı saklayın (`mv trader.anchor trader.anchor.<tarih>`), olayı kayda geçirin, ardından `traderd store anchor-init` çalıştırın.
5. Servisleri başlatın.

## Hata mesajları

| Mesaj | Anlamı | Ne yapılır |
| --- | --- | --- |
| `schema is older than this binary` | Migration çalıştırılmamış | `traderd store migrate` (sahip rol) |
| `schema is newer than this binary` | Veritabanı daha yeni bir sürümle migrate edilmiş | Doğru imaj sürümünü deploy edin |
| `migration N does not match` / `was changed after it was applied` | Uygulanmış bir migration dosyası değişmiş | Migration'lar değiştirilmez, yeni migration eklenir |
| `trigger ... missing / disabled / calls ...`, `function ... differs` | Bir koruma kaldırılmış ya da değiştirilmiş | Durun; kimin değiştirdiğini bulun; dışa aktarmayla karşılaştırın |
| `has more privileges` | `traderd` sahip, süper kullanıcı ya da fazladan yetkili rolle bağlanıyor | `dsn_file` uygulama rolünü göstermeli |
| `hash chain broken` | Bir olay satırı değişmiş ya da silinmiş | Durun; son geçerli dışa aktarma ve yedekle karşılaştırın |
| `event log ends before the anchor` | Eski bir anlık görüntü geri yüklenmiş ya da olaylar silinmiş | Yukarıdaki geri yükleme adımı 4 |
| `anchor file missing for a non-empty event log` | Bu hostun çapası yok (yeni ya da yeniden kurulmuş sunucu) | Zinciri VPS dışı dışa aktarmayla karşılaştırın, sonra `store anchor-init` |
| `fenced` | Eski bir yürütücü epoch'u ile yazma denendi | Beklenen koruma; yalnız güncel kira sahibi yazar |
| `requires sslmode=verify-full and sslrootcert` | Uzak veritabanına doğrulamasız ya da açık CA'sız bağlantı (çok hostlu dizelerde her host için) | Dizeye `sslmode=verify-full&sslrootcert=/run/secrets/rds-ca.pem` ekleyin |
| `can create temporary tables` | Uygulama rolünde `TEMPORARY` yetkisi var | `traderd store migrate` çalıştırın (yetkiyi PUBLIC'ten ve uygulama rolünden alır) |
| `payload ... needs an integer "v"` / `not UTF-8 JSON` | Sözleşmeye uymayan olay | Yazan kodu düzeltin; veritabanı reddeder |
