# Runbook: depolama (olay günlüğü ve yedek)

Veritabanı `store.path` (örnek: `data/trader.db`), yanında çapa dosyası `trader.db.anchor`. İkisi birlikte bir bütündür.

## Bütünlük modeli

- `events` tablosu yalnız eklenir. Her satır bir öncekine hash zinciriyle bağlıdır: `SHA-256(prev_hash ‖ seq ‖ ts_ns ‖ kind ‖ payload)` (sabit genişlikli tam sayılar, uzunluk önekli dizgiler). İlk satırın `prev_hash`'i 32 sıfır bayttır.
- Tetikleyiciler `UPDATE`/`DELETE`'i ve `seq ≠ max+1` eklemelerini reddeder; `recursive_triggers` açık olduğu için `INSERT OR REPLACE` de geçemez. `fills`, `approvals`, `tax_ledger` da değiştirilemez.
- Her commit'ten sonra zincirin ucu (seq, hash) çapa dosyasına atomik yazılır. Çapa, bir çökme anında veritabanının birkaç olay gerisinde kalabilir; bu kabul edilir. Veritabanı çapadan önce bitiyorsa (kesilmiş ya da eski kopya) ya da çapadaki hash tutmuyorsa doğrulama başarısız olur.
- Payload'lar `"v"` sürüm alanı taşıyan JSON nesneleridir; saklanan baytlar hash'lenen baytlardır.
- Doğrulama, koruyucu tetikleyicilerin ve `events` tablosunun SQL'ini migration'ların ürettiği SQL ile birebir karşılaştırır ve uygulanmış migration'ların SHA-256'sını kontrol eder.
- Aynı anda yalnız bir süreç yazar (`trader.db.lock`).

**Tehdit modeli.** Bu düzen kazaları (kesilme, yanlış ya da eski kopyanın geri yüklenmesi, depoyu atlayan araçlarla yapılan değişiklikler) ve bozulmayı yakalar. Hem veritabanını hem çapa dosyasını yazabilen kötü niyetli bir yerel kullanıcıyı durdurmaz; buna karşı savunma, yedeklerin ve çapaların VPS dışındaki kopyalarıdır (W7).

## Komutlar

```sh
traderd store verify -config configs/example.yaml        # store.path'i doğrular, dosyayı değiştirmez
traderd store verify -file data/backups/trader-X.db      # bir yedeği doğrular (.sha256 varsa önce o)
traderd store backup -config configs/example.yaml        # backup_dir/trader-<UTC zaman>.db (+ .anchor, .sha256)
traderd preflight   -config configs/example.yaml -credentials ~/.config/trader/binance.json -symbols BTCUSDT,ETHUSDT
```

## Yedek

- `traderd store backup` kaynağı **salt okuma** bağlantıyla açar: migration çalıştırmaz, kaynağa yazmaz; `traderd` çalışırken güvenle alınabilir. `VACUUM INTO` tutarlı bir anlık görüntü üretir.
- Yedeğin çapası yedeğin kendi zincirinin ucundan yazılır; kaynak çapası (kopyadan önce okunur) yedekte aynı sırada aynı hash'i göstermelidir. Ardından SHA-256 yazılır ve yedek doğrulanır. Dosyalar baştan `0600` oluşturulur; bir hata olursa yarım dosyalar silinir.
- Ana dosyayı `cp` ile kopyalamayın: WAL modunda `-wal` dosyasındaki son işlemler kopyada olmaz. Her zaman `traderd store backup` kullanın.
- Şifreleme ve VPS dışına taşıma W7'de eklenir.

## Geri yükleme

1. `traderd` ve `killswitch` servislerini durdurun.
2. Yedeği **açmadan önce** doğrulayın: `traderd store verify -file data/backups/trader-X.db` (önce `.sha256`, sonra zincir). Bir veritabanını yazma için açmak dosyayı değiştirir ve sağlama toplamını geçersiz kılar.
3. Yedeği ve **kendi** `.anchor` dosyasını birlikte `store.path` konumuna kopyalayın (`trader-X.db` → `trader.db`, `trader-X.db.anchor` → `trader.db.anchor`). Eski `-wal`/`-shm` dosyalarını silin.
4. `traderd store verify -config ...` çalıştırın.
5. Servisi başlatın. Başlangıç mutabakatı (W2b) borsadaki gerçek durumu okur; mutabakat temiz çıkmadan yeni emir verilmez.

Eski bir yedeği güncel çapayla birlikte açmaya çalışırsanız doğrulama `ErrAnchorAhead` ile reddeder. Bu bilinçli bir korumadır.

## Hata durumları

| Hata | Anlamı | Eylem |
| --- | --- | --- |
| `hash chain broken` | Bir satır değiştirilmiş, silinmiş ya da yeniden sıralanmış | Servisi başlatmayın; son doğrulanan yedekten geri yükleyin; olayı inceleyin |
| `event log ends before the anchor` | Veritabanı kesilmiş ya da eski bir kopya | Doğru yedeği kendi çapasıyla geri yükleyin |
| `anchor file missing` | Boş olmayan veritabanının çapası yok | Yedeği çapasıyla birlikte geri yükleyin |
| `schema integrity check failed` | Koruyucu tetikleyici silinmiş ya da uygulanmış bir migration değişmiş | Servisi başlatmayın; inceleyin |
| `newer than this binary` | Veritabanı daha yeni bir sürümle açılmış | Doğru ikili sürümü kullanın |
| `open for writing by another process` | Başka bir süreç veritabanını yazma için açık tutuyor | Çalışan `traderd`'yi kontrol edin; yedek için `store backup` zaten salt okuma çalışır |
| `events committed but anchor not updated` | Olaylar kalıcı, çapa yazılamadı (disk/izin) | Disk ve izinleri düzeltin; bir sonraki açılış çapayı günceller |
| preflight `database missing while its anchor exists` | Veritabanı silinmiş ya da `store.path` yanlış | Yolu düzeltin ya da yedekten geri yükleyin; boş bir günlükle başlamayın |

Göreli `store.path` ve `store.backup_dir`, config dosyasının bulunduğu klasöre göre çözülür (çalışma dizinine göre değil).
