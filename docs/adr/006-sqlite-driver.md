# ADR-006: SQLite sürücüsü modernc.org/sqlite

- Durum: **Yerini aldı: ADR-007** (2026-11-19; kullanıcı SQLite'ı reddetti, PostgreSQL seçildi)
- Önceki durum: Kabul edildi (2026-11-19, kullanıcı onayı U-3)
- Kaynak: ADR-002; `research/E_binance_impl_facts.md` §J

## Bağlam

ADR-002 SQLite'ı (WAL) seçti, sürücüyü sonraya bıraktı. İki aday var: `modernc.org/sqlite` (saf Go) ve `github.com/mattn/go-sqlite3` (cgo).

## Karar

`modernc.org/sqlite` v1.62.0 (SQLite 3.54.1 gömülü; `go 1.26` ister).

- cgo ve C derleyicisi gerekmez; `CGO_ENABLED=0` ile tek dosya ikili üretilir ve VPS'e kopyalanır.
- `modernc.org/libc` sürümü sürücünün `go.mod`'undakiyle birebir aynı olmalı (v1.80.1). Sürücünün kendi dokümanı bu bağımlılığı "kırılgan" diye niteliyor; CI eşleşmeyi kontrol eder.
- Linux'ta OFD kilitleri (`sqlite.OFDLocking(true)`) ilk bağlantıdan önce koddan açılır. Böylece süreçteki ilgisiz bir `os.File` kapanışı veritabanı dosyasındaki işlem kilitlerini düşüremez. Sürücü dokümanına göre (`ofd.go`) `-shm` dosyasının kilitleri POSIX kalır.
- Tek yazıcı kuralı ayrıca bir kilit dosyasıyla (`<db>.lock`, `flock`) zorlanır; ikinci bir yazıcı süreç `Open`'da reddedilir.

## Değerlendirilen alternatif

- **mattn/go-sqlite3 v1.14.53:** daha yaygın, ama cgo + gcc ister; çapraz derleme ve statik ikili zorlaşır.

## Sonuçlar

- CPU yoğun sorgularda C sürümünden 1,3–2 kat yavaş (sürücü dokümanı). Yazma hacmi düşük olduğu için önemsiz.
- Sürücü yükseltmesi her zaman `libc` sürümüyle birlikte yapılır.
