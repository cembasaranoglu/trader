# ADR-002: Depolama için SQLite (WAL)

- Durum: **Yerini aldı: ADR-007** (2026-11-19). Olay günlüğü, hash zinciri ve çapa ilkeleri ADR-007'de PostgreSQL üzerinde sürer.
- Önceki durum: Kabul edildi (2026-11-19)
- Kaynak: Plan dokümanı, "Repo yapısı ve konfigürasyon" bölümü

## Bağlam

Tek bir VPS üzerinde tek bir süreç; yazma hacmi düşük (dakikada en fazla birkaç karar ve emir). Karar günlüğü, işlem/vergi defteri, politika sürümleri ve haftalar süren paper/canlı geçmişi kalıcı tutulmalı ve VPS dışına yedeklenebilmeli.

## Karar

WAL modunda SQLite. Karar günlüğü append-only tutulur ve kayıtlar hash zinciriyle bağlanır.

## Değerlendirilen alternatif

- **PostgreSQL:** ayrı servis, bakım ve yedekleme yükü getirir; bu ölçekte gerekli değil.

## Sonuçlar

- Yedekleme tek dosyanın tutarlı bir kopyasıdır (online backup API ya da `VACUUM INTO`); VPS dışına şifreli kopyalanır (W7).
- Kill switch ayrı bir süreç olduğu için veritabanına yazmaz; kendi olaylarını ayrı bir dosyaya yazar ve ana süreç onları içe alır. Bu, tek yazıcı kuralını korur.
- Yazma hacmi ya da eşzamanlı yazıcı ihtiyacı ölçümle artarsa karar W8'de yeniden değerlendirilir.
- Sürücü seçimi (cgo'lu ya da saf Go) W1'de, bağımlılık gerekçesiyle birlikte ayrı bir notla yapılır.
