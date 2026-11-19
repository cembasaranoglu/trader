# Runbook: depolama (olay günlüğü ve yedek)

Veritabanı `store.path` (örnek: `data/trader.db`), yanında çapa dosyası `trader.db.anchor`. İkisi birlikte bir bütündür.

## Bütünlük modeli

- `events` tablosu yalnız eklenir. Her satır bir öncekine hash zinciriyle bağlıdır: `SHA-256(prev_hash ‖ seq ‖ ts_ns ‖ kind ‖ payload)` (sabit genişlikli tam sayılar, uzunluk önekli dizgiler). İlk satırın `prev_hash`'i 32 sıfır bayttır.
- Tetikleyiciler `UPDATE`/`DELETE`'i ve `seq ≠ max+1` eklemelerini reddeder; `recursive_triggers` açık olduğu için `INSERT OR REPLACE` de geçemez. `fills`, `approvals`, `tax_ledger` da değiştirilemez.
- Her commit'ten sonra zincirin ucu (seq, hash) çapa dosyasına atomik yazılır. Çapa, bir çökme anında veritabanının birkaç olay gerisinde kalabilir; bu kabul edilir. Veritabanı çapadan önce bitiyorsa (kesilmiş ya da eski kopya) ya da çapadaki hash tutmuyorsa doğrulama başarısız olur.
- Payload'lar `"v"` sürüm alanı taşıyan JSON nesneleridir; saklanan baytlar hash'lenen baytlardır.

## Komutlar

```sh
traderd store verify -config configs/example.yaml   # zinciri ve çapayı doğrular, dosyayı değiştirmez
traderd store backup -config configs/example.yaml   # backup_dir/trader-<UTC zaman>.db (+ .anchor, .sha256)
traderd preflight   -config configs/example.yaml -credentials ~/.config/trader/binance.json -symbols BTCUSDT,ETHUSDT
```

## Yedek

- `VACUUM INTO` ile tutarlı kopya alınır, yazmalar bu sırada bekler. Yedeğin yanına çapası ve SHA-256'sı yazılır, yedek açılıp doğrulanır. Dosyalar `0600`.
- Ana dosyayı `cp` ile kopyalamayın: WAL modunda `-wal` dosyasındaki son işlemler kopyada olmaz. Her zaman `traderd store backup` kullanın.
- Şifreleme ve VPS dışına taşıma W7'de eklenir.

## Geri yükleme

1. `traderd` ve `killswitch` servislerini durdurun.
2. Yedeği ve **kendi** `.anchor` dosyasını birlikte `store.path` konumuna kopyalayın (`trader-X.db` → `trader.db`, `trader-X.db.anchor` → `trader.db.anchor`). Eski `-wal`/`-shm` dosyalarını silin.
3. `traderd store verify` çalıştırın.
4. Servisi başlatın. Başlangıç mutabakatı (W2b) borsadaki gerçek durumu okur; mutabakat temiz çıkmadan yeni emir verilmez.

Eski bir yedeği güncel çapayla birlikte açmaya çalışırsanız doğrulama `ErrAnchorAhead` ile reddeder. Bu bilinçli bir korumadır.

## Hata durumları

| Hata | Anlamı | Eylem |
| --- | --- | --- |
| `hash chain broken` | Bir satır değiştirilmiş, silinmiş ya da yeniden sıralanmış | Servisi başlatmayın; son doğrulanan yedekten geri yükleyin; olayı inceleyin |
| `event log ends before the anchor` | Veritabanı kesilmiş ya da eski bir kopya | Doğru yedeği kendi çapasıyla geri yükleyin |
| `anchor file missing` | Boş olmayan veritabanının çapası yok | Yedeği çapasıyla birlikte geri yükleyin |
| `schema integrity check failed` | Koruyucu tetikleyici silinmiş ya da uygulanmış bir migration değişmiş | Servisi başlatmayın; inceleyin |
| `newer than this binary` | Veritabanı daha yeni bir sürümle açılmış | Doğru ikili sürümü kullanın |
