# W1c test senaryoları — Depolama, olay günlüğü, yedek, ön kontroller

Hazırlayan: bağımsız test tasarımcısı (koda bakılmadan, yalnız spesifikasyon ve resmi dokümandan). Tarih: 2026-11-19.

## Kaynak kısaltmaları

| Kısaltma | Dosya / bölüm |
| --- | --- |
| `SPEC#n` | `docs/plan/01-waves.md`, W1c tablosu, satır `n` (ör. `SPEC#3(e)`) |
| `E-n` | `docs/plan/01-waves.md`, "Emir yolu tasarım kuralları" |
| `ADR-002` | `docs/adr/002-storage-sqlite.md` |
| `ADR-001` | `docs/adr/001-architecture-and-invariants.md` (I-6 değiştirilemez günlük, I-7 çekim yetkisi yok) |
| `E§J`, `E§D` | `research/E_binance_impl_facts.md` bölüm J (SQLite sürücüleri), D (test ortamları) |
| `REST:<bölüm>` | `/tmp/bdocs/rest-api.md` |
| `ENUM`, `ERR` | `/tmp/bdocs/enums.md`, `/tmp/bdocs/errors.md` |
| `BIN` | `docs/exchanges/binance.md` (apiRestrictions alanları, ölçümler) |
| `OVR` | `docs/plan/00-overview.md` (üretime hazır tanımı) |

Tür etiketleri: `[unit]` CI'da çalışır (geçici dizinde gerçek SQLite dosyası, sahte REST sunucusu; alt süreç testleri dahil); `[fuzz]` Go fuzz hedefi; `[live]` gerçek Binance erişimi gerektirir (`//go:build live`, CI dışında; ÖLÇÜM-2'ye göre `api.binance.com` ve `demo-api.binance.com` GitHub runner'dan 451 döner).

Ortak test sabitleri: `TS0 = 1795132800000` (2026-11-20 00:00:00.000 UTC). `ops.max_clock_skew_ms = 500`, `execution.stp_mode = EXPIRE_TAKER` (configs/example.yaml).

## Spesifikasyondaki boşluklar (önce okunmalı)

1. **Hash yalnız `prev_hash ‖ kanonik payload` kapsıyor.** `seq`, `ts`, `kind` hash'e girmiyor (SPEC#3(b)). Bu alanlarda yapılan kurcalama zincirle tespit edilemez. Öneri: hash girdisi kanonik zarf `{seq, ts, kind, v, payload}` olsun. CHAIN-05/06 bu öneriye göre "tespit edilmeli" bekler; spesifikasyon değişmezse bu iki test "bilinen sınırlama" olarak işaretlenir.
2. **Kuyruk kesme ve tam yeniden yazma tespit edilemez.** Sondan satır silmek (trigger düşürülerek) ya da tüm zinciri yeniden hesaplayıp yazmak `VerifyChain`'i geçer; dış bir çapa (son `seq` + `hash`'in DB dışında saklanması: yedek manifesti, preflight çıktısı, ayrı dosya) gerekir. CHAIN-09/10.
3. **Genesis `prev_hash` tanımlı değil.** Sabit bir değer (ör. 32 sıfır bayt) dokümante edilmeli; CHAIN-01 bunu sabitler.
4. **`‖` birleştirmesi belirsiz.** E-1'de `policy_version ‖ symbol ‖ signal_kind ‖ close_time` ayraçsız birleştirilirse `("v1","BTCUSDT")` ile `("v1B","TCUSDT")` aynı baytları üretir. Uzunluk öneki ya da ayraç gerekir; zaman damgasının kodlaması (ondalık dizgi mi 8 bayt big-endian mi) yazılmalı. INT-05. Olay zincirinde `prev_hash` sabit uzunlukta (32 bayt) olduğu sürece ayraçsız birleştirme güvenlidir; `prev_hash`'in ham bayt mı hex mi girdiği yazılmalı.
5. **`INSERT OR REPLACE` DELETE trigger'ını atlayabilir.** SQLite'ta REPLACE çakışma çözümü satırı silerken DELETE trigger'ları yalnız `recursive_triggers` açıksa tetiklenir (SQLite dokümanı, `lang_conflict.html`; sağlanan doküman setinde değil). Append-only koruması için ayrıca bir INSERT trigger'ı (`seq = max(seq)+1` ve `prev_hash = son hash`) ya da REPLACE'i engelleyen başka bir yol gerekir. EVT-04.
6. **`/sapi` Demo'da çalışıyor mu bilinmiyor.** Testnet'te çalışmadığı belgeli (E§D: "Only /api endpoints work; /sapi does not"). Spesifikasyon: Demo'da atlanırsa açıkça yazılır (SPEC#6). Atlamaya yalnız Demo/testnet ortamında izin verilmeli; canlı hostta `/sapi` erişilemezse ret. PRE-KEY-14/15.
7. **apiRestrictions alan kümesi sağlanan resmi dokümanlarda yok.** BIN yalnız `ipRestrict`, `enableReading`, `enableSpotAndMarginTrading`, `enableWithdrawals` "ve diğerleri" diyor; tam liste Binance wallet dokümanında. Testler bilinmeyen `enable*`/`permits*` boolean alanı `true` ise reddi (fail-closed) bekler.
8. **Saat sapması ölçüm belirsizliği.** Gidiş-dönüş süresi (BIN: tek örnekte 1419 ms) yarısı 500 ms eşiğini aşarsa sapma eşik içinde doğrulanamaz; spesifikasyon bu durumu tanımlamıyor. PRE-CLK-05.
9. **Preflight'ta DB yoksa ne olur** tanımlı değil. PRE-CHAIN-03.
10. **Birden fazla süreç aynı DB'yi yazıcı olarak açarsa** ne olacağı tanımlı değil (ADR-002 tek yazıcı kuralı). DB-08.

---

## 1. Config ve komut satırı (CFG)

1. **W1C-CFG-01 — `store.path`, `store.backup_dir` zorunlu** `[unit]`
   - Girdi: (a) ikisi var; (b) `store.path` yok; (c) `store.backup_dir` yok; (d) bilinmeyen `store.paht`.
   - Beklenen: (a) kabul; (b)(c)(d) alan yoluyla ret; varsayılan yok.
   - Kaynak: SPEC#14; configs/example.yaml başlığı ("no defaults").
2. **W1C-CFG-02 — Yol doğrulaması** `[unit]`
   - Girdi: `store.path: ""`, göreli yol `data/t.db`, `~` içeren yol, `store.path == store.backup_dir`, `backup_dir` içinde `store.path`.
   - Beklenen: boş ret; göreli yol: savunmacı, config dosyasına göre çözülür ya da ret (karar dokümante); `~` genişletilmez (ya da açıkça genişletilir — dokümante); yedek dizini DB dosyasının kendisi olamaz.
   - Kaynak: SPEC#14; savunmacı.
3. **W1C-CFG-03 — Şema ve örnek config senkronu** `[unit]`
   - Beklenen: `configs/schema.json` ve `configs/example.yaml` yeni alanları içerir; mevcut kurallar değişmez.
   - Kaynak: SPEC#10.
4. **W1C-CFG-04 — Alt komut yüzeyi** `[unit]`
   - Girdi: `traderd preflight -config f -credentials c`; `traderd store verify -db f`; `traderd store backup -db f -out o`; `traderd store` (alt-alt komut yok); `traderd store verify` (`-db` yok).
   - Beklenen: ilk üçü çalışır; son ikisi kullanım mesajı ve sıfırdan farklı çıkış kodu.
   - Kaynak: SPEC#3(f), SPEC#12.
5. **W1C-CFG-05 — `store verify` var olmayan DB oluşturmaz** `[unit]`
   - Girdi: `traderd store verify -db /tmp/x/yok.db`.
   - Beklenen: tipli "dosya yok" hatası, sıfırdan farklı çıkış; dosya oluşmaz (SQLite varsayılan olarak boş dosya yaratır; salt okuma modu `mode=ro` ile açılmalı).
   - Kaynak: SPEC#12; savunmacı (SQLite açılış davranışı).
6. **W1C-CFG-06 — `store verify` salt okuma** `[unit]`
   - Girdi: çalışan bir yazıcı süreç varken `store verify`.
   - Beklenen: yazıcıyı bloklamaz (WAL okuyucusu), DB'ye yazmaz (dosya mtime ve `PRAGMA data_version` değişmez), sonuç anlık görüntüye göre.
   - Kaynak: SPEC#3(a) (WAL, okuma havuzu); SPEC#12.

## 2. Veritabanı açma, PRAGMA'lar, bağlantı modeli (DB)

7. **W1C-DB-01 — PRAGMA değerleri** `[unit]`
   - Girdi: store açılır; yazıcı bağlantıda `PRAGMA journal_mode`, `PRAGMA synchronous`, `PRAGMA busy_timeout` sorgulanır; okuma havuzundaki her bağlantıda da `journal_mode` ve `busy_timeout`.
   - Beklenen: `wal`; `2` (FULL); `busy_timeout` > 0 ve dokümante değer. `synchronous` bağlantı başına olduğundan yazıcıda FULL olması şart.
   - Kaynak: SPEC#3(a) ("WAL, busy_timeout, synchronous=FULL"); E§J (DSN `_pragma`).
8. **W1C-DB-02 — Tek yazıcı bağlantı** `[unit]`
   - Girdi: 50 goroutine aynı anda olay ekler.
   - Beklenen: yazıcı havuzu `MaxOpenConns=1`; "database is locked" hatası yok; tüm eklemeler başarılı; `seq` 1..50 boşluksuz.
   - Kaynak: SPEC#3(a); E§J ("single writer connection and a separate read pool").
9. **W1C-DB-03 — Okuma havuzu yazamaz** `[unit]`
   - Girdi: okuma havuzundan `INSERT INTO events …`.
   - Beklenen: hata (`query_only` ya da `mode=ro`); veri değişmez.
   - Kaynak: SPEC#3(a); savunmacı.
10. **W1C-DB-04 — Okuma havuzu sınırlı** `[unit]`
    - Beklenen: okuma havuzu `SetMaxOpenConns` ile sınırlı (dokümante değer); 100 eşzamanlı okuma sınırı aşmaz.
    - Kaynak: E§J ("Bound the pool with SetMaxOpenConns").
11. **W1C-DB-05 — `busy_timeout` içinde kilit bırakılır** `[unit]`
    - Ön koşul: harici bir süreç (ya da ham bağlantı) `BEGIN IMMEDIATE` ile yazma kilidi tutar.
    - Girdi: kilit `busy_timeout/2` sonra bırakılır; store bir olay ekler.
    - Beklenen: ekleme bekleyip başarılı olur.
    - Kaynak: SPEC#3(a).
12. **W1C-DB-06 — `busy_timeout` aşılır** `[unit]`
    - Girdi: kilit `busy_timeout × 2` tutulur.
    - Beklenen: tipli `ErrBusy` (errors.Is); kısmi satır yok; zincir bozulmaz; sonraki ekleme kilit bırakılınca başarılı ve `seq` boşluksuz devam eder.
    - Kaynak: SPEC#3(a); OVR madde 3 (tipli hata).
13. **W1C-DB-07 — Disk dolu (SQLITE_FULL)** `[unit]`
    - Girdi: `PRAGMA max_page_count` düşük ayarlanır (disk dolu benzetimi); olaylar eklenir.
    - Beklenen: tipli yazma hatası; son başarılı olaydan sonra zincir sağlam; kısmi işlem yok. (Güvenli moda geçiş W2c.)
    - Kaynak: SPEC#19 ("disk dolması → yazma hatası güvenli moda geçirir (W2c)").
14. **W1C-DB-08 — İkinci yazıcı süreç** `[unit]`
    - Girdi: aynı `store.path` ile iki süreç store açar ve yazmaya çalışır.
    - Beklenen: tanımsız (boşluk 10). Savunmacı: ikinci süreç açılışta tipli hata ile reddedilir (kilit dosyası / `locking_mode=EXCLUSIVE` vb.); en azından zincir çatallanmaz (aynı `prev_hash`'e iki olay bağlanmaz).
    - Kaynak: ADR-002 ("tek yazıcı kuralı"); savunmacı.
15. **W1C-DB-09 — Bozuk DB dosyası** `[unit]`
    - Girdi: `store.path` rastgele baytlarla dolu 4 KB dosya; sıfır baytlık dosya; SQLite olmayan başka bir DB.
    - Beklenen: rastgele bayt: tipli "not a database" hatası, dosya üzerine yazılmaz; sıfır bayt: savunmacı, yeni DB olarak başlatılır ya da ret (dokümante); süreç başlamaz/başlar kararı açık.
    - Kaynak: savunmacı.
16. **W1C-DB-10 — Dosya izinleri** `[unit]`
    - Beklenen: yeni DB dosyası ve `-wal`/`-shm` dosyaları `0600` (ya da dokümante umask); başka kullanıcıya okunur değil (vergi ve emir verisi).
    - Kaynak: savunmacı (OVR güvenlik maddesi).
17. **W1C-DB-11 — Sürücü sürüm eşleşmesi (modernc seçilirse)** `[unit]`
    - Girdi: `go.mod`.
    - Beklenen: `modernc.org/libc` sürümü, `modernc.org/sqlite`'ın kendi go.mod'unda istediği sürümle birebir (v1.62.0 için v1.80.1); CI kontrolü uyumsuzlukta kırmızı. `CGO_ENABLED=0 go build ./...` geçer.
    - Kaynak: SPEC#10, SPEC#16; E§J ("Pin exactly the same modernc.org/libc version").

## 3. Migration'lar (MIG)

18. **W1C-MIG-01 — Boş DB'ye ileri migration** `[unit]`
    - Girdi: yeni dosya.
    - Beklenen: `0001_init.sql` uygulanır; tüm tablolar (`events`, `intents`, `orders`, `fills`, `balances_snapshot`, `approvals`, `tax_ledger`) ve trigger'lar var; şema sürümü = 1 (sürüm tablosu ya da `user_version`).
    - Kaynak: SPEC#3(a)(b), SPEC#9.
19. **W1C-MIG-02 — Çift çalıştırma idempotent** `[unit]`
    - Girdi: store iki kez açılıp kapatılır.
    - Beklenen: ikinci açılışta migration uygulanmaz; şema ve veri değişmez; hata yok.
    - Kaynak: SPEC#15 ("Migration ileri/çift").
20. **W1C-MIG-03 — Kısmi uygulanmış DB'ye yeni migration** `[unit]`
    - Girdi: sürüm 1 DB + test için gömülü sahte `0002_*.sql`.
    - Beklenen: yalnız 0002 uygulanır; veri korunur; sürüm 2.
    - Kaynak: SPEC#3(a) ("gömülü ileri-yalnız migration'lar").
21. **W1C-MIG-04 — Migration hatası atomik** `[unit]`
    - Girdi: sahte `0002` ikinci ifadesinde sözdizimi hatası.
    - Beklenen: işlem geri alınır; sürüm 1 kalır; ilk ifadenin etkisi yok; tipli hata; süreç başlamaz.
    - Kaynak: SPEC#3(a); savunmacı.
22. **W1C-MIG-05 — DB sürümü ikili dosyadan yeni** `[unit]`
    - Girdi: sürüm 3 DB, ikili yalnız 1..2 biliyor.
    - Beklenen: tipli "şema yeni, geri alma yok" hatası; DB'ye dokunulmaz.
    - Kaynak: SPEC#20 ("İleri-yalnız migration; veri yedekten").
23. **W1C-MIG-06 — Uygulanmış migration içeriği değişti** `[unit]`
    - Girdi: sürüm 1 DB'de kayıtlı migration özeti ile gömülü `0001` içeriği farklı.
    - Beklenen: spesifikasyon sessiz; savunmacı: migration özeti saklanır ve uyuşmazlıkta tipli ret.
    - Kaynak: savunmacı.
24. **W1C-MIG-07 — Numara boşluğu / tekrarı** `[unit]`
    - Girdi: gömülü dosyalar `0001`, `0003` (0002 yok) ya da iki `0002`.
    - Beklenen: test/derleme aşamasında başarısız (çalışma zamanında sessizce atlanmaz).
    - Kaynak: savunmacı.
25. **W1C-MIG-08 — Eşzamanlı iki migrator** `[unit]`
    - Girdi: iki süreç aynı boş DB'yi aynı anda açar.
    - Beklenen: migration bir kez uygulanır; diğeri bekler (busy) ya da DB-08 gereği reddedilir; şema tek ve doğru.
    - Kaynak: savunmacı.

## 4. Olay günlüğü ve append-only (EVT)

26. **W1C-EVT-01 — Ekleme alanları** `[unit]`
    - Girdi: `Append(kind="test.ping", payload={"v":1,"x":"a"})`, `ts=TS0`.
    - Beklenen: satır `seq=1`, `ts=1795132800000` (UTC ms, tamsayı; yerel saat dilimi `TZ=Europe/Istanbul` olsa da aynı), `kind`, `payload_json` kanonik, `prev_hash`=genesis, `hash`=SHA-256(genesis ‖ kanonik payload).
    - Kaynak: SPEC#3(b).
27. **W1C-EVT-02 — UPDATE trigger ile reddedilir** `[unit]`
    - Girdi: yazıcı bağlantıdan `UPDATE events SET payload_json='{}' WHERE seq=1`; `UPDATE events SET seq=seq` (değer değişmeyen).
    - Beklenen: ikisi de trigger hatasıyla reddedilir; satır değişmez.
    - Kaynak: SPEC#3(b) ("UPDATE/DELETE trigger ile engelli"), SPEC#15 ("trigger reddi").
28. **W1C-EVT-03 — DELETE trigger ile reddedilir** `[unit]`
    - Girdi: `DELETE FROM events WHERE seq=1`; `DELETE FROM events` (koşulsuz — SQLite'ın truncate optimizasyonu trigger varken devre dışıdır).
    - Beklenen: ikisi de reddedilir; satır sayısı değişmez.
    - Kaynak: SPEC#3(b).
29. **W1C-EVT-04 — `INSERT OR REPLACE` ve UPSERT** `[unit]`
    - Girdi: `INSERT OR REPLACE INTO events(seq,…) VALUES(1,…)` (mevcut seq); `INSERT … ON CONFLICT(seq) DO UPDATE SET payload_json=…`; `REPLACE INTO events …`.
    - Beklenen: üçü de reddedilir, satır 1 değişmez. REPLACE için DELETE trigger'ı `recursive_triggers` olmadan tetiklenmez (boşluk 5); test bu yolu özellikle doğrular.
    - Kaynak: SPEC#3(b); boşluk 5.
30. **W1C-EVT-05 — Doğrudan INSERT ile sahte satır** `[unit]`
    - Girdi: API'yi atlayarak `INSERT INTO events(seq,ts,kind,payload_json,prev_hash,hash) VALUES(2,…,'x','{}',X'00…',X'00…')`.
    - Beklenen: savunmacı: INSERT trigger'ı (`prev_hash` = son hash, `seq` = son+1) reddeder; yoksa `VerifyChain` seq 2'de hata verir (CHAIN-03 ile çift güvence).
    - Kaynak: boşluk 5; SPEC#3(c).
31. **W1C-EVT-06 — Kind enum** `[unit]`
    - Girdi: kayıtlı olmayan `kind="foo.bar"`; boş kind; 1 KB kind.
    - Beklenen: Append tipli hata ile reddeder; satır yazılmaz; `seq` ilerlemez.
    - Kaynak: SPEC#13 ("Olay kind enum").
32. **W1C-EVT-07 — Payload sürümü `v`** `[unit]`
    - Girdi: `v` yok; `v:"1"` (dizgi); `v:0`; `v:1.5`; `v:2` (kind için bilinmeyen sürüm).
    - Beklenen: yazımda `v` zorunlu pozitif tamsayı; bilinmeyen sürüm yazımda ret. Okumada bilinmeyen `v` olan eski/yeni satır `VerifyChain`'i bozmaz (hash şemadan bağımsız).
    - Kaynak: SPEC#13 ("payload sürümü v").
33. **W1C-EVT-08 — Kanonik JSON kuralları** `[unit]`
    - Girdi: aynı nesne farklı anahtar sırası `{"b":1,"a":2}` ve `{"a":2,"b":1}`; boşluklu; `"<>&"` içeren dizgi; `"é"` ve `"é"`; büyük tamsayı `9007199254740993`; ondalık `0.1`; iç içe nesne.
    - Beklenen: aynı nesne → aynı bayt dizisi ve aynı hash. Kurallar dokümante ve test vektörüyle sabit: anahtarlar sıralı, boşluksuz, HTML kaçışı yok (Go `encoding/json` varsayılan olarak `<>&` kaçışlar — kanonik biçim bunu sabitler), UTF-8 normal biçim kuralı, tamsayılar kayıpsız (float64'e dönüşmez). Para değerleri dizgi olarak taşınır (float yok).
    - Kaynak: SPEC#13 ("kanonik JSON"); savunmacı (kural seti yazılı değil).
34. **W1C-EVT-09 — Kanonik olmayan payload reddi** `[unit]`
    - Girdi: yinelenen anahtar `{"a":1,"a":2}`; NaN; geçersiz UTF-8; 10 MB payload; 1000 seviye iç içe.
    - Beklenen: yinelenen anahtar, NaN, geçersiz UTF-8 ret; boyut ve derinlik sınırları dokümante, aşımda ret.
    - Kaynak: savunmacı.
35. **W1C-EVT-10 — Kanonik JSON fuzz** `[fuzz]`
    - Girdi: rastgele JSON.
    - Beklenen: `canon(canon(x)) == canon(x)`; `parse(canon(x))` anlamca `x`; panik yok.
    - Kaynak: SPEC#13.
36. **W1C-EVT-11 — `ts` monoton değil** `[unit]`
    - Girdi: duvar saati geri alınmışken iki olay: `ts=TS0`, sonra `ts=TS0−1000`.
    - Beklenen: spesifikasyon sessiz; savunmacı: ikisi de kabul (sıra kaynağı `seq`), uyarı olayı; `VerifyChain` geçer.
    - Kaynak: savunmacı.
37. **W1C-EVT-12 — Eşzamanlı eklemede zincir çatallanmaz** `[unit]`
    - Girdi: `-race` altında 16 goroutine × 500 olay.
    - Beklenen: `seq` 1..8000 boşluksuz; her satırın `prev_hash`'i bir öncekinin `hash`'i; `VerifyChain` geçer.
    - Kaynak: SPEC#3(a)(b).
38. **W1C-EVT-13 — İşlem içinde ekleme geri alınırsa** `[unit]`
    - Girdi: olay + intent aynı işlemde; intent UNIQUE ihlali nedeniyle işlem geri alınır.
    - Beklenen: olay da yazılmaz; bellekteki "son hash" önbelleği (varsa) geri alınan satırın hash'ine ilerlemez; sonraki ekleme doğru `prev_hash` kullanır.
    - Kaynak: SPEC#3(b); savunmacı (önbellek tutarsızlığı klasik hata).

## 5. Hash zinciri ve `VerifyChain` (CHAIN)

39. **W1C-CHAIN-01 — Bilinen cevap vektörü** `[unit]`
    - Girdi: genesis (dokümante, ör. 32 sıfır bayt) + payload `{"v":1}`.
    - Beklenen: `hash` = SHA-256(genesis_bytes ‖ `{"v":1}` baytları), testte sabit hex olarak yazılı; `prev_hash`'in ham bayt mı hex mi girdiği sabitlenir.
    - Kaynak: SPEC#3(b); boşluk 3, 4.
40. **W1C-CHAIN-02 — Sağlam zincir** `[unit]`
    - Girdi: 10 000 olay; boş tablo.
    - Beklenen: `VerifyChain` nil hata; boş tabloda nil hata; süre makul (benchmark).
    - Kaynak: SPEC#3(c).
41. **W1C-CHAIN-03 — Payload kurcalama** `[unit]`
    - Ön koşul: 100 olay; trigger'lar test içinde düşürülür (saldırgan dosya erişimi benzetimi).
    - Girdi: `seq=50` `payload_json` içinde `"x":"a"` → `"x":"b"`.
    - Beklenen: `VerifyChain` tipli `ErrChainBroken{Seq:50, Reason:"hash"}`.
    - Kaynak: SPEC#15 ("zincir kurcalama (payload…)").
42. **W1C-CHAIN-04 — Anlamca aynı ama baytça farklı payload** `[unit]`
    - Girdi: `seq=50` payload'una boşluk eklenir ya da anahtar sırası değiştirilir.
    - Beklenen: tespit edilir (hash saklanan baytlardan ya da yeniden kanonikleştirilmiş biçimden — her iki durumda da uyuşmazlık); savunmacı: saklanan bayt kanonik değilse ayrı neden `"noncanonical"`.
    - Kaynak: SPEC#3(b).
43. **W1C-CHAIN-05 — `kind` kurcalama** `[unit]`
    - Girdi: `seq=50` `kind` `order.submitted` → `order.canceled`.
    - Beklenen: boşluk 1: spesifikasyon olduğu gibi uygulanırsa **tespit edilemez**. Öneri benimsenirse `ErrChainBroken{Seq:50}`. Test hangi davranışın geçerli olduğunu açıkça sabitler.
    - Kaynak: SPEC#3(b); boşluk 1.
44. **W1C-CHAIN-06 — `ts` kurcalama** `[unit]`
    - Girdi: `seq=50` `ts` +1 sa.
    - Beklenen: CHAIN-05 ile aynı.
    - Kaynak: SPEC#3(b); boşluk 1.
45. **W1C-CHAIN-07 — Sıra kurcalama (iki satırın yer değiştirmesi)** `[unit]`
    - Girdi: `seq=50` ve `seq=51` satırlarının payload/hash/prev_hash değerleri takas edilir; ikinci varyant: yalnız `seq` numaraları takas edilir.
    - Beklenen: ikisinde de en küçük bozuk `seq` raporlanır (50).
    - Kaynak: SPEC#15 ("sıra").
46. **W1C-CHAIN-08 — Ortadan silme** `[unit]`
    - Girdi: `seq=50` silinir.
    - Beklenen: `ErrChainBroken{Seq:51, Reason:"prev_hash"}` ve/veya `"seq_gap"`.
    - Kaynak: SPEC#15 ("silme").
47. **W1C-CHAIN-09 — Kuyruk kesme** `[unit]`
    - Girdi: son 10 satır (`seq=91..100`) silinir.
    - Beklenen: boşluk 2: zincir tek başına tespit edemez. Dış çapa varsa (ör. son yedek manifestindeki `last_seq=100,last_hash=…` ya da preflight'ın kaydettiği çapa) → `ErrChainTruncated{Expected:100, Got:90}`. Çapa yoksa test "bilinen sınırlama" olarak işaretlenir ve runbook'a yazılır.
    - Kaynak: SPEC#15 ("silme"); boşluk 2.
48. **W1C-CHAIN-10 — Tam zincir yeniden yazımı** `[unit]`
    - Girdi: `seq=50` değiştirilir ve 50..100 tüm hash'ler yeniden hesaplanır.
    - Beklenen: zincir iç tutarlı → yalnız dış çapa ile tespit edilebilir (`last_hash` uyuşmaz). CHAIN-09 ile aynı sınırlama.
    - Kaynak: boşluk 2.
49. **W1C-CHAIN-11 — İlk satır kurcalama** `[unit]`
    - Girdi: `seq=1` `prev_hash` genesis dışı bir değere çevrilir; ikinci varyant `seq=1` silinir.
    - Beklenen: `ErrChainBroken{Seq:1, Reason:"genesis"}`; silmede `seq=2`'nin prev_hash'i genesis değil → tespit; ya da `seq` 1'den başlamıyor → tespit.
    - Kaynak: SPEC#3(c); boşluk 3.
50. **W1C-CHAIN-12 — `hash` sütunu bozuk biçim** `[unit]`
    - Girdi: `hash` NULL; 31 bayt; hex dizgi yerine metin `"zz"`.
    - Beklenen: tipli hata, panik yok, ilgili `seq` raporlanır. Şemada `NOT NULL` ve uzunluk `CHECK`'i varsa ekleme aşamasında da reddedilir.
    - Kaynak: savunmacı.
51. **W1C-CHAIN-13 — Trigger'ların varlığı doğrulanır** `[unit]`
    - Girdi: trigger'lar düşürülmüş ama veri sağlam DB.
    - Beklenen: savunmacı: `VerifyChain` (ya da `store verify`) trigger'ların eksik olduğunu ayrı uyarı/hatayla raporlar (`sqlite_master` kontrolü).
    - Kaynak: SPEC#3(b); savunmacı.
52. **W1C-CHAIN-14 — Akış halinde doğrulama, bellek sınırı** `[unit]`
    - Girdi: 1 000 000 olay.
    - Beklenen: `VerifyChain` satırları akışla okur (tüm tabloyu belleğe almaz); context iptaline uyar ve hızla döner.
    - Kaynak: OVR madde 3 (context); savunmacı.
53. **W1C-CHAIN-15 — Kurcalama fuzz** `[fuzz]`
    - Girdi: sağlam 50 olaylık zincir; rastgele satır/sütun/bayt değişikliği (trigger'sız).
    - Beklenen: payload, prev_hash, hash ya da seq değişikliği her zaman tespit edilir; raporlanan `seq` ≤ değiştirilen ilk satır; panik yok. (`kind`/`ts` boşluk 1'e göre hariç tutulabilir.)
    - Kaynak: SPEC#3(c).

## 6. Intent'ler ve önce-yaz kuralı (INT)

54. **W1C-INT-01 — UNIQUE ihlali tipli hata** `[unit]`
    - Girdi: aynı `intent_id` ile iki ekleme.
    - Beklenen: ikinci ekleme `errors.Is(err, store.ErrDuplicateIntent)`; tek satır; ilk satır değişmez; ilgili olay yazılmaz (EVT-13).
    - Kaynak: SPEC#3(b) ("UNIQUE(intent_id)"), SPEC#15; E-1.
55. **W1C-INT-02 — Yeniden başlatma sonrası yinelenen intent** `[unit]`
    - Girdi: intent eklenir, store kapatılır (ya da süreç öldürülür), yeniden açılır, aynı intent yeniden eklenir.
    - Beklenen: `ErrDuplicateIntent`; yeni intent yok.
    - Kaynak: E-1 ("aynı sinyalin yeniden başlatmadan sonra yeniden üretilmesi yeni intent oluşturmaz").
56. **W1C-INT-03 — Eşzamanlı aynı intent** `[unit]`
    - Girdi: 10 goroutine aynı `intent_id`'yi aynı anda ekler.
    - Beklenen: tam 1 başarı, 9 `ErrDuplicateIntent`.
    - Kaynak: E-1.
57. **W1C-INT-04 — Intent ID belirlenimci** `[unit]`
    - Girdi: `policy_version="v0000"`, `symbol="BTCUSDT"`, `signal_kind="momentum"`, `close_time=1795132859999`, iki kez hesaplanır; ayrıca `close_time` 1 ms farklı.
    - Beklenen: ilk ikisi aynı 32 bayt; üçüncüsü farklı. Test vektörü sabit hex ile yazılır (fonksiyon W1c'de değil W2a'da ise test orada; W1c yalnız UNIQUE'i sınar).
    - Kaynak: E-1.
58. **W1C-INT-05 — Birleştirme çakışması** `[unit]`
    - Girdi: `("v1","BTCUSDT","m",T)` ve `("v1B","TCUSDT","m",T)`; `("v1","BTC","USDTm",T)` ve `("v1","BTCUSDT","m",T)`.
    - Beklenen: farklı ID'ler (boşluk 4; ayraçsız birleştirme bu testi geçemez).
    - Kaynak: E-1; boşluk 4.
59. **W1C-INT-06 — Önce yaz: intent + client order ID + stop/TP aynı işlemde ve kalıcı** `[unit]`
    - Girdi: `SaveIntent{intent_id, client_order_id, stop, take_profit}` döner dönmez alt süreç `SIGKILL` ile öldürülür (CRASH-02 düzeneği).
    - Beklenen: yeniden açılışta dört alanın hepsi var (synchronous=FULL ile commit kalıcı); hiçbiri yarım yazılmamış.
    - Kaynak: E-2 ("intent, client order ID, hedef stop ve kar al fiyatları emir gönderilmeden önce depoya yazılır (fsync)").
60. **W1C-INT-07 — Client order ID tekilliği** `[unit]`
    - Girdi: iki farklı intent aynı `client_order_id` ile.
    - Beklenen: savunmacı: `orders`/`intents` tablosunda `client_order_id` UNIQUE; ikinci ret (E-3: aynı ID asla yeniden gönderilmez — depo bunu yapısal olarak engellemeli).
    - Kaynak: E-3; spesifikasyon tablo kısıtını yazmıyor (savunmacı).
61. **W1C-INT-08 — Gönderim sonucu ayrı olay** `[unit]`
    - Girdi: intent kaydı + ardından ayrı `order.submit_result` olayı.
    - Beklenen: intent satırı sonuçla UPDATE edilmez ya da ediliyorsa intents tablosu append-only kapsamında değildir — karar dokümante. Olay günlüğünde iki ayrı satır.
    - Kaynak: E-2 ("Gönderim sonucu ayrı olay").

## 7. Diğer tablolar (SCHEMA)

62. **W1C-SCHEMA-01 — Tablo ve sütun varlığı** `[unit]`
    - Girdi: `PRAGMA table_info` her tablo için.
    - Beklenen: `events(seq, ts, kind, payload_json, prev_hash, hash)`; `tax_ledger` sütunları: zaman, sembol, yön, miktar, fiyat, ücret, ücret varlığı, USD/TRY kuru, kur kaynağı, işlem vergisi oranı; `intents`, `orders`, `fills`, `balances_snapshot`, `approvals` mevcut.
    - Kaynak: SPEC#3(b).
63. **W1C-SCHEMA-02 — Para alanları kayıpsız** `[unit]`
    - Girdi: `tax_ledger`'a miktar `"0.00001000"`, fiyat `"65432.10000000"`, ücret `"0.00000001"` yazılıp okunur.
    - Beklenen: bayt bayt aynı dizgi (ya da tam sayı + ölçek); `REAL` sütun yok, float yuvarlaması yok.
    - Kaynak: savunmacı (vergi defteri doğruluğu; spesifikasyon tipi yazmıyor).
64. **W1C-SCHEMA-03 — Kur kaynağı zorunlu** `[unit]`
    - Girdi: `tax_ledger` satırı USD/TRY kuru dolu, kaynak boş.
    - Beklenen: savunmacı: `NOT NULL`/`CHECK` ile ret.
    - Kaynak: SPEC#3(b) ("USD/TRY kuru ve kaynağı").
65. **W1C-SCHEMA-04 — Zaman sütunları UTC ms** `[unit]`
    - Beklenen: tüm zaman sütunları INTEGER UTC ms; yerel saat dilimli metin yok; `TZ` ortam değişkeni sonucu etkilemez.
    - Kaynak: savunmacı.
66. **W1C-SCHEMA-05 — `approvals` ve `fills` değiştirilemezliği** `[unit]`
    - Beklenen: spesifikasyon trigger'ı yalnız `events` için istiyor; `approvals`, `fills`, `tax_ledger` için UPDATE/DELETE politikası dokümante edilir (savunmacı öneri: aynı trigger). Test seçilen politikayı sabitler.
    - Kaynak: SPEC#1 ("Kararların, emirlerin, dolumların, onayların … değiştirilemez kaydı (I-6)").

## 8. Çökme tutarlılığı (CRASH)

67. **W1C-CRASH-01 — Yazma döngüsü ortasında SIGKILL** `[unit]`
    - Ön koşul: alt süreç (test ikili dosyası `-test.run` ile yeniden çağrılır) sürekli olay ekler, her commit sonrası `seq`'i stdout'a yazar.
    - Girdi: rastgele 50–500 ms sonra `SIGKILL`; 20 tekrar.
    - Beklenen: her tekrarda yeniden açılış başarılı; `PRAGMA integrity_check` = `ok`; `VerifyChain` geçer; ebeveynin gördüğü son `seq` DB'de var (onaylanan commit kaybolmaz); DB'deki son `seq` ≥ görülen son `seq` (en fazla bir commit stdout'a yazılmadan kalmış olabilir); yarım satır yok.
    - Kaynak: SPEC#15 ("alt süreç SIGKILL sonrası tutarlılık"); SPEC#3(a) (synchronous=FULL).
68. **W1C-CRASH-02 — Çok ifadeli işlem ortasında SIGKILL** `[unit]`
    - Girdi: işlem: intent + order + event; ifadeler arasında test kancasıyla bekleme; bekleme sırasında `SIGKILL`.
    - Beklenen: yeniden açılışta üçünden hiçbiri yok (atomiklik).
    - Kaynak: SPEC#3(a); E-2.
69. **W1C-CRASH-03 — Migration ortasında SIGKILL** `[unit]`
    - Girdi: sahte uzun `0002` uygulanırken öldürme.
    - Beklenen: yeniden açılışta sürüm 1 ve `0002` yeniden uygulanır, ya da sürüm 2 tam; ara durum yok.
    - Kaynak: SPEC#3(a).
70. **W1C-CRASH-04 — Checkpoint ortasında SIGKILL ve WAL kurtarma** `[unit]`
    - Girdi: büyük WAL (10 000 olay), `wal_checkpoint(TRUNCATE)` sırasında öldürme.
    - Beklenen: yeniden açılışta tüm commit'li olaylar var; `-wal` dosyası kurtarılır; `VerifyChain` geçer.
    - Kaynak: SPEC#3(a) (WAL).
71. **W1C-CRASH-05 — `-wal`/`-shm` dosyası kayıp ya da yalnız ana dosya kopyalanmış** `[unit]`
    - Girdi: SIGKILL sonrası yalnız `.db` dosyası başka dizine kopyalanıp açılır (operatör hatası benzetimi).
    - Beklenen: commit'lerin bir kısmı eksik olabilir (WAL'de kalanlar); store açılır ve `VerifyChain` geçer (önek tutarlı) — bu senaryo runbook'ta "yedek için cp kullanmayın, `store backup` kullanın" notunu gerekçelendirir.
    - Kaynak: SPEC#3(d); ADR-002 ("tutarlı bir kopya").
72. **W1C-CRASH-06 — Güç kesintisi (fsync)** `[live]`
    - Girdi: VPS'te `dm-flakey` ya da sanal makine anlık durdurma ile yazma sırasında kesinti (CI'da yapılamaz; Binance gerektirmez ama gerçek donanım/VM gerektirir — "live" yerine "manual" olarak da işaretlenebilir).
    - Beklenen: CRASH-01 ile aynı değişmezler.
    - Kaynak: SPEC#3(a) (synchronous=FULL).

## 9. Yedek ve geri yükleme (BAK)

73. **W1C-BAK-01 — `VACUUM INTO` + SHA-256** `[unit]`
    - Girdi: 1000 olaylı DB; `traderd store backup -db f -out o.db`.
    - Beklenen: `o.db` oluşur; yanında SHA-256 (ör. `o.db.sha256`, biçim dokümante: `<hex>  o.db`); `sha256sum -c` geçer; çıkış kodu 0; stdout'ta hash ve son `seq`/`hash` (dış çapa, CHAIN-09).
    - Kaynak: SPEC#3(d), SPEC#12.
74. **W1C-BAK-02 — Yedeğin açılıp doğrulanması** `[unit]`
    - Girdi: `o.db` store ile açılır.
    - Beklenen: `integrity_check=ok`; migration sürümü aynı; `VerifyChain` geçer; olay sayısı ve son hash kaynakla aynı; trigger'lar yedekte de var.
    - Kaynak: SPEC#15 ("yedeğin açılıp doğrulanması").
75. **W1C-BAK-03 — Eşzamanlı yazma sırasında yedek** `[unit]`
    - Girdi: yazıcı saniyede 200 olay eklerken yedek alınır.
    - Beklenen: yedek tutarlı anlık görüntü: `seq` 1..K boşluksuz, `VerifyChain` geçer; yazıcı yedek boyunca dokümante süreden uzun bloklanmaz (ya da bloklanma süresi ölçülüp raporlanır).
    - Kaynak: SPEC#3(d); ADR-002.
76. **W1C-BAK-04 — Hedef dosya var** `[unit]`
    - Girdi: `-out` var olan dosya (boş ve dolu iki varyant).
    - Beklenen: üzerine yazılmaz; tipli hata. (SQLite `VACUUM INTO`'nun mevcut dosya davranışı sağlanan dokümanlarda yok; savunmacı: komut kendisi önceden kontrol eder.)
    - Kaynak: savunmacı.
77. **W1C-BAK-05 — Hedef = kaynak, hedef dizini yok, yazılamaz** `[unit]`
    - Girdi: `-out` = `-db`; `-out /yok/dizin/o.db`; salt okunur dizin.
    - Beklenen: üçü de DB'ye dokunmadan tipli hata.
    - Kaynak: savunmacı.
78. **W1C-BAK-06 — Yedek ortasında SIGKILL** `[unit]`
    - Girdi: büyük DB'de `store backup` süreci öldürülür.
    - Beklenen: yarım dosya "başarılı yedek" gibi görünmez: savunmacı, geçici adla yazılır ve tamamlanınca atomik `rename`; `.sha256` yalnız tamamlanan yedek için var; kaynak DB etkilenmez.
    - Kaynak: savunmacı.
79. **W1C-BAK-07 — Bozulmuş yedek tespiti** `[unit]`
    - Girdi: yedek dosyasında tek bayt çevrilir.
    - Beklenen: SHA-256 doğrulaması başarısız; geri yükleme reddedilir.
    - Kaynak: SPEC#3(d).
80. **W1C-BAK-08 — Geri yükleme ve zincire devam** `[unit]`
    - Girdi: yedek yeni `store.path`'e kopyalanır (runbook adımı), store açılır, 10 olay eklenir.
    - Beklenen: yeni olaylar yedeğin son hash'ine bağlanır; `VerifyChain` geçer; ileri migration varsa uygulanır.
    - Kaynak: SPEC#20 ("veri yedekten"); SPEC#9 (`docs/runbook/storage.md`).
81. **W1C-BAK-09 — Yedek dosya izinleri** `[unit]`
    - Beklenen: yedek ve `.sha256` `0600`; `store.backup_dir` yoksa oluşturulur mu ret mi dokümante.
    - Kaynak: savunmacı (şifreleme W7'de; o zamana kadar dosya izinleri tek koruma).
82. **W1C-BAK-10 — Boş DB yedeği** `[unit]`
    - Girdi: yalnız migration'ları uygulanmış, olaysız DB.
    - Beklenen: yedek başarılı, açılır, `VerifyChain` geçer, son `seq`=0 raporlanır.
    - Kaynak: SPEC#3(d).

## 10. Preflight (PRE)

Sıra: config → `SyncTime` → `KeyPermissions` → STP → zincir doğrulama (SPEC#3(e)). Her ret nedeni ayrı tipli hata (SPEC#15). Beklenti: ilk başarısız adımda durulur; sonraki adımlar çağrılmaz (sahte REST'te çağrı sayısı 0) — spesifikasyon ok (→) ile sıralı akış tanımlıyor.

### 10.1 Genel ve config

83. **W1C-PRE-GEN-01 — Mutlu yol** `[unit]`
    - Ön koşul: geçerli config; sahte REST: `/api/v3/time` sapma 54 ms; apiRestrictions `{"ipRestrict":false,"enableReading":true,"enableSpotAndMarginTrading":false,"enableWithdrawals":false}` + bilinen diğer alanlar false; exchangeInfo BTCUSDT `allowedSelfTradePreventionModes:["NONE","EXPIRE_TAKER","EXPIRE_MAKER","EXPIRE_BOTH"]`; sağlam zincir.
    - Girdi: `traderd preflight -config f -credentials c -symbols BTCUSDT` (ya da sembol `marketdata`'dan).
    - Beklenen: çıkış kodu 0; her adım "OK" satırı; adımlar listelenen sırada çağrıldı.
    - Kaynak: SPEC#3(e), SPEC#12.
84. **W1C-PRE-GEN-02 — Geçersiz config ağ çağrısı yapmaz** `[unit]`
    - Girdi: config'te `max_clock_skew_ms: 0` ya da bilinmeyen anahtar.
    - Beklenen: `ErrConfig`; sahte REST'e 0 çağrı; çıkış kodu ≠ 0.
    - Kaynak: SPEC#3(e) (sıra).
85. **W1C-PRE-GEN-03 — Her ret ayrı tip ve ayrı çıkış kodu/mesajı** `[unit]`
    - Girdi: aşağıdaki her ret senaryosu.
    - Beklenen: `errors.Is` ile ayırt edilebilir tipler (`ErrConfig`, `ErrClockSkew`, `ErrWithdrawalsEnabled`, `ErrExtraPermissions`, `ErrTradingWithoutIPRestriction`, `ErrPermissionFieldInvalid`, `ErrPermissionsUnavailable`, `ErrSTPModeNotAllowed`, `ErrChainBroken`, …); mesajda neden ve ilgili alan/sembol/seq.
    - Kaynak: SPEC#15 ("preflight: her ret nedeni ayrı tipli hata").
86. **W1C-PRE-GEN-04 — Çıktıda sır yok** `[unit]`
    - Girdi: başarılı ve başarısız koşular; ayrıca sunucu hata gövdesi API anahtarını yankılar.
    - Beklenen: stdout/stderr/log içinde API anahtarı, gizli anahtar, imza, tam istek URL'si (imzalı sorgu dizgisi) yok; yankılanan değer maskelenir.
    - Kaynak: OVR madde 5–6 ("Loglarda sır yok").
87. **W1C-PRE-GEN-05 — Kimlik dosyası sertleştirmesi** `[unit]`
    - Girdi: `-credentials` dosyası `0644`; sembolik bağ; yok; bozuk JSON.
    - Beklenen: mevcut venuecheck sertleştirmesiyle aynı davranış (izin geniş → ret vb.); tipli hata; ağ çağrısı yok.
    - Kaynak: 01-waves §1 ("kimlik dosyası sertleştirmesi").
88. **W1C-PRE-GEN-06 — Ağ zaman aşımı ve 5XX fail-closed** `[unit]`
    - Girdi: her adımın uç noktası sırayla (a) yanıt vermez, (b) 500, (c) 503, (d) bağlantı reddi.
    - Beklenen: sınırlı yeniden deneme + context zaman aşımı; sonunda ilgili adım için tipli ret (preflight geçmez); asla "bilinmiyor → geç" yok.
    - Kaynak: OVR madde 3; REST:HTTP Return Codes (5XX durum bilinmez).

### 10.2 Saat (CLK)

89. **W1C-PRE-CLK-01 — Sapma sınırları** `[unit]`
    - Girdi: sahte sunucu saati yerelden `+500`, `+501`, `−500`, `−501`, `0` ms (RTT ≈ 0).
    - Beklenen: `±500` ve `0` geçer; `±501` `ErrClockSkew{OffsetMs:…, LimitMs:500}`. Sınır kapsayıcılığı dokümante.
    - Kaynak: SPEC#3(e) (`SyncTime`, R-20); configs/example.yaml (`max_clock_skew_ms: 500`).
90. **W1C-PRE-CLK-02 — Ofset hesabı RTT ortası** `[unit]`
    - Girdi: istek gönderim `t0`, yanıt `t1 = t0 + 400 ms`, `serverTime = t0 + 200 + 300`.
    - Beklenen: ofset = `serverTime − (t0+t1)/2` = +300 ms → geçer. RTT monotonik saatle ölçülür.
    - Kaynak: REST:Check server time; savunmacı (yöntem yazılı değil).
91. **W1C-PRE-CLK-03 — Çoklu örnek, en düşük RTT** `[unit]`
    - Girdi: 5 örnek; RTT'ler 1419, 120, 90, 800, 95 ms; düşük RTT örnekleri ofset +60.
    - Beklenen: savunmacı: en düşük RTT örneği (ya da medyan) kullanılır, ofset ≈ +60 → geçer. Yöntem dokümante.
    - Kaynak: BIN (ölçüm: medyan +54 ms, en yüksek RTT 1419 ms).
92. **W1C-PRE-CLK-04 — Bozuk `serverTime`** `[unit]`
    - Girdi: `{"serverTime":"abc"}`, `{}`, `{"serverTime":0}`, `{"serverTime":-1}`, mikrosaniye `1795132800000000`.
    - Beklenen: tipli ret (`ErrClockSkew` değil `ErrServerTimeInvalid` ya da eşdeğeri); mikrosaniye değeri (header istenmediği için beklenmez) makul aralık kontrolüyle ret.
    - Kaynak: REST:General ("milliseconds by default"; mikrosaniye yalnız `X-MBX-TIME-UNIT` ile).
93. **W1C-PRE-CLK-05 — Belirsizlik eşikten büyük** `[unit]`
    - Girdi: tüm örneklerde RTT ≥ 1200 ms (belirsizlik ±600 ms > 500).
    - Beklenen: boşluk 8; savunmacı: "sapma doğrulanamadı" tipli ret (fail-closed) ya da dokümante uyarı. Seçim test ile sabitlenir.
    - Kaynak: boşluk 8.
94. **W1C-PRE-CLK-06 — Ölçüm sırasında duvar saati sıçraması** `[unit]`
    - Girdi: örnekler arasında yerel saat +2 sn adım.
    - Beklenen: RTT monotonik saatten; ofset duvar saatinden; sıçrama sonrası örnekler tutarsızsa yeniden ölçüm; negatif RTT yok.
    - Kaynak: savunmacı.
95. **W1C-PRE-CLK-07 — Saat hatası sonraki adımları çağırmaz** `[unit]`
    - Girdi: sapma +900 ms.
    - Beklenen: `ErrClockSkew`; `/sapi/v1/account/apiRestrictions` çağrısı 0 (imzalı istek `-1021` alırdı: "Timestamp for this request was 1000ms ahead"); exchangeInfo çağrısı 0.
    - Kaynak: SPEC#3(e) (sıra); ERR:-1021.

### 10.3 Anahtar yetkileri (KEY)

`GET /sapi/v1/account/apiRestrictions` yanıtı (alan adları BIN'den; tam liste sağlanan dokümanlarda yok, boşluk 7). Temel yanıt `R0 = {"ipRestrict":false,"enableReading":true,"enableSpotAndMarginTrading":false,"enableWithdrawals":false, <diğer bilinen enable*/permits* alanları: false>}`.

96. **W1C-PRE-KEY-01 — Salt okuma anahtarı geçer** `[unit]`
    - Girdi: `R0`.
    - Beklenen: geçer (IP kısıtı yok ama işlem yetkisi de yok).
    - Kaynak: SPEC#3(e); SPEC#18 ("yalnız okuma anahtarı").
97. **W1C-PRE-KEY-02 — Çekim açık** `[unit]`
    - Girdi: `R0` + `enableWithdrawals:true` (+ `ipRestrict:true`, çünkü BIN'e göre çekim ancak IP kısıtıyla açılabilir; iki varyant da sınanır).
    - Beklenen: her iki varyantta `ErrWithdrawalsEnabled`; süreç başlamaz.
    - Kaynak: SPEC#3(e) ("çekim açık … → başlama (I-7)"); ADR-001 I-7; OVR madde 6.
98. **W1C-PRE-KEY-03 — Fazladan yetki (`OtherEnabled` dolu)** `[unit]`
    - Girdi: `R0` + tek tek her bilinen ek yetki alanı `true` (ör. wallet dokümanındaki margin, futures, iç transfer, evrensel transfer, opsiyon, portföy marjin, FIX alanları — tam liste kodda dokümante edilir).
    - Beklenen: her biri `ErrExtraPermissions{Fields:[…]}`; alan adı mesajda.
    - Kaynak: SPEC#3(e) ("OtherEnabled dolu").
99. **W1C-PRE-KEY-04 — Bilinmeyen yeni yetki alanı** `[unit]`
    - Girdi: `R0` + `"enableFooTrading":true`; ikinci varyant `"permitsBar":true`; üçüncü `"enableFooTrading":false`.
    - Beklenen: savunmacı fail-closed: ilk ikisi `ErrExtraPermissions`; üçüncüsü geçer. Boolean olmayan bilinmeyen alan (ör. `"createTime":1795132800000`, `"tradingAuthorityExpirationTime":…`) yok sayılır.
    - Kaynak: boşluk 7; savunmacı.
100. **W1C-PRE-KEY-05 — İşlem açık, IP kısıtsız** `[unit]`
     - Girdi: `R0` + `enableSpotAndMarginTrading:true`, `ipRestrict:false`.
     - Beklenen: `ErrTradingWithoutIPRestriction`.
     - Kaynak: SPEC#3(e) ("işlem yetkisi IP kısıtsız → başlama"); BIN (UI kuralı: işlem yetkisi IP kısıtı sonrası açılabiliyor — yine de istemci kontrol eder).
101. **W1C-PRE-KEY-06 — İşlem açık, IP kısıtlı** `[unit]`
     - Girdi: `R0` + `enableSpotAndMarginTrading:true`, `ipRestrict:true`.
     - Beklenen: geçer (W1c'de config `mode: paper` olsa bile; işlem anahtarı bu adımda yasak değil). Not: W1c canlı doğrulaması salt okuma anahtarıyla yapılır.
     - Kaynak: SPEC#3(e).
102. **W1C-PRE-KEY-07 — Zorunlu alan eksik** `[unit]`
     - Girdi: `R0`'dan sırasıyla `enableWithdrawals`, `ipRestrict`, `enableSpotAndMarginTrading`, `enableReading` çıkarılır; ayrıca boş nesne `{}`.
     - Beklenen: her biri `ErrPermissionFieldInvalid{Field:…}`; eksik alan `false` sayılmaz (Go sıfır değer tuzağı; `*bool` ya da varlık kontrolü).
     - Kaynak: savunmacı (fail-closed; I-7).
103. **W1C-PRE-KEY-08 — Alan tipi yanlış** `[unit]`
     - Girdi: `"enableWithdrawals":"false"`, `0`, `null`, `[]`; `"ipRestrict":"true"`.
     - Beklenen: hepsi `ErrPermissionFieldInvalid`; dizgi `"false"` false olarak yorumlanmaz.
     - Kaynak: savunmacı.
104. **W1C-PRE-KEY-09 — Yinelenen anahtar / bozuk JSON / dizi gövde** `[unit]`
     - Girdi: `{"enableWithdrawals":false,"enableWithdrawals":true}`; kesik JSON; `[]`; `null` gövde; 10 MB gövde.
     - Beklenen: yinelenen anahtar: savunmacı ret (Go `encoding/json` son değeri alır — `true` olsa geçmemeli, `false` son olsa da güvenli tarafta ret); diğerleri tipli ret; mevcut gövde sınırı uygulanır.
     - Kaynak: 01-waves §1 (gövde sınırı, null gövde reddi); savunmacı.
105. **W1C-PRE-KEY-10 — `enableReading:false`** `[unit]`
     - Girdi: `R0` + `enableReading:false`.
     - Beklenen: spesifikasyon sessiz; savunmacı: ret (sistem hesap okuyamaz; mutabakat imkânsız) — ayrı tip ya da `ErrPermissionFieldInvalid`.
     - Kaynak: savunmacı.
106. **W1C-PRE-KEY-11 — Birden çok ihlal** `[unit]`
     - Girdi: çekim açık + işlem IP kısıtsız + fazladan yetki.
     - Beklenen: savunmacı: tüm ihlaller tek raporda listelenir (operatör tek seferde düzeltebilsin), birincil hata tipi en ağır olan (`ErrWithdrawalsEnabled`); `errors.Is` her biri için true (errors.Join).
     - Kaynak: SPEC#15; savunmacı.
107. **W1C-PRE-KEY-12 — Binance hata kodları** `[unit]`
     - Girdi: HTTP 401/400 gövdeleri: `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`, `{"code":-2014,…}`, `{"code":-1022,…}`, `{"code":-1021,…}`.
     - Beklenen: her biri tipli ret (`ErrKeyRejected`, `ErrSignature`, `ErrTimestamp`); yeniden deneme döngüsü yok (`-1021` için en fazla bir `SyncTime` + tek tekrar, dokümante).
     - Kaynak: ERR:-2015, -2014, -1022, -1021.
108. **W1C-PRE-KEY-13 — 429/418 ve host geri çekilmesi** `[unit]`
     - Girdi: apiRestrictions 429 `Retry-After: 30`.
     - Beklenen: preflight `Retry-After`'ı aşan süre beklemez; tipli ret ya da sınırlı bekleme (dokümante); imzalı host geri çekilmesi piyasa hostunu etkilemez (W1b REST-08 ile tutarlı).
     - Kaynak: REST:IP Limits.
109. **W1C-PRE-KEY-14 — Demo'da `/sapi` yok → açıkça atlanır** `[unit]`
     - Ön koşul: config/credentials ortamı Demo (`demo-api.binance.com`).
     - Girdi: `/sapi/v1/account/apiRestrictions` 404 (ya da 403/HTML gövde).
     - Beklenen: adım `SKIPPED` olarak, gerekçesiyle ("/sapi Demo'da kullanılamıyor; anahtar yetkileri doğrulanmadı") stdout'ta ve olay günlüğünde açıkça yazılır; sessizce "OK" yazılmaz; diğer adımlar devam eder; çıkış kodu dokümante (0 + uyarı ya da ayrı kod).
     - Kaynak: SPEC#6 ("preflight Demo'da yetki adımını atlarsa bunu açıkça yazar"); E§D.
110. **W1C-PRE-KEY-15 — Canlı hostta `/sapi` yok → ret** `[unit]`
     - Ön koşul: ortam canlı (`api.binance.com`).
     - Girdi: `/sapi` 404.
     - Beklenen: `ErrPermissionsUnavailable`; atlama yok. Atlama kararı yanıt koduna değil yalnız yapılandırılmış ortama bağlıdır (saldırgan/proxy 404 döndürerek I-7 kontrolünü atlatamaz).
     - Kaynak: SPEC#3(e); ADR-001 I-7; savunmacı.
111. **W1C-PRE-KEY-16 — Demo'da `/sapi` çalışıyorsa normal değerlendirilir** `[unit]`
     - Girdi: Demo ortamı, `/sapi` 200 + `enableWithdrawals:true`.
     - Beklenen: `ErrWithdrawalsEnabled` (atlama yalnız uç erişilemezse).
     - Kaynak: SPEC#6.
112. **W1C-PRE-KEY-17 — Testnet ortamı** `[unit]`
     - Girdi: ortam testnet.
     - Beklenen: E§D'ye göre `/sapi` çalışmaz → KEY-14 ile aynı açık atlama (testnet destekleniyorsa); destek yoksa config reddi.
     - Kaynak: E§D ("Only /api endpoints work; /sapi does not").

### 10.4 STP modu (STP)

113. **W1C-PRE-STP-01 — İzinli mod** `[unit]`
     - Girdi: `stp_mode=EXPIRE_TAKER`; BTCUSDT `allowedSelfTradePreventionModes:["EXPIRE_MAKER","EXPIRE_TAKER"]`.
     - Beklenen: geçer.
     - Kaynak: SPEC#3(e); REST:Exchange information; BIN ("STP Varsayılan EXPIRE_MAKER; EXPIRE_TAKER izinli").
114. **W1C-PRE-STP-02 — Mod sembolde izinli değil** `[unit]`
     - Girdi: BTCUSDT `["NONE"]` (doküman örneğindeki gibi).
     - Beklenen: `ErrSTPModeNotAllowed{Symbol:"BTCUSDT", Mode:"EXPIRE_TAKER", Allowed:["NONE"]}`.
     - Kaynak: SPEC#3(e) ("allowedSelfTradePreventionModes ∌ execution.stp_mode → başlama (R-22)").
115. **W1C-PRE-STP-03 — Çoklu sembol, biri ihlalde** `[unit]`
     - Girdi: BTCUSDT izinli, ETHUSDT `["EXPIRE_MAKER"]`, SOLUSDT izinli.
     - Beklenen: ret; mesajda yalnız ETHUSDT (savunmacı: tüm ihlal eden semboller listelenir).
     - Kaynak: SPEC#3(e).
116. **W1C-PRE-STP-04 — Liste boş / alan yok / null** `[unit]`
     - Girdi: `[]`; alan yok; `null`.
     - Beklenen: üçü de ret (fail-closed; boş liste "her şey izinli" değil).
     - Kaynak: savunmacı.
117. **W1C-PRE-STP-05 — Büyük/küçük harf ve bilinmeyen değerler** `[unit]`
     - Girdi: `["expire_taker"]`; `["EXPIRE_TAKER","TRANSFER","FUTURE_MODE"]`.
     - Beklenen: enum büyük/küçük harf duyarlı → ilki ret; ikincisi geçer (bilinmeyen ek değer yok sayılır).
     - Kaynak: ENUM:STP Modes (`NONE, EXPIRE_MAKER, EXPIRE_TAKER, EXPIRE_BOTH, DECREMENT, TRANSFER`).
118. **W1C-PRE-STP-06 — Sembol exchangeInfo'da yok** `[unit]`
     - Girdi: `-symbols BTCUSDT,NOTREAL`; sahte REST `exchangeInfo?symbols=[…]` → `{"code":-1121,"msg":"Invalid symbol."}`.
     - Beklenen: tipli ret (`ErrUnknownSymbol{NOTREAL}`); STP kontrolü atlanmış sayılmaz.
     - Kaynak: REST:Exchange information ("If the value provided to symbol or symbols do not exist, the endpoint will throw an error").
119. **W1C-PRE-STP-07 — Sembol kaynağı** `[unit]`
     - Girdi: (a) yalnız komut satırı sembolleri; (b) yalnız `marketdata` config sembolleri (varsa); (c) ikisi birden.
     - Beklenen: kontrol edilen küme dokümante (birleşim); hiç sembol yoksa savunmacı: ret ya da açık "kontrol edilecek sembol yok" uyarısı — sessizce geçmez.
     - Kaynak: SPEC#3(e) ("marketdata/komut satırındaki sembollerde"; W2a evrene genişletir).
120. **W1C-PRE-STP-08 — exchangeInfo ağırlığı** `[unit]`
     - Beklenen: tek `exchangeInfo?symbols=[…]` isteği (ağırlık 20) — sembol başına ayrı istek değil; piyasa hostu kullanılır (imzasız uç).
     - Kaynak: REST:Exchange information (Weight 20).

### 10.5 Zincir adımı (CHAIN)

121. **W1C-PRE-CHAIN-01 — Kırık zincir → başlama** `[unit]`
     - Girdi: CHAIN-03 düzeneğiyle `seq=50` kurcalanmış DB.
     - Beklenen: önceki adımlar geçer; `ErrChainBroken{Seq:50}`; çıkış ≠ 0.
     - Kaynak: SPEC#3(e) ("→ zincir doğrulama").
122. **W1C-PRE-CHAIN-02 — Sağlam ve boş zincir** `[unit]`
     - Beklenen: ikisi de geçer.
     - Kaynak: SPEC#3(c)(e).
123. **W1C-PRE-CHAIN-03 — DB dosyası yok** `[unit]`
     - Girdi: `store.path` var olmayan dosya.
     - Beklenen: boşluk 9; savunmacı: preflight DB oluşturmaz, "henüz depo yok" açık durum (ilk kurulum) olarak raporlar; karar dokümante ve test sabitler.
     - Kaynak: boşluk 9.
124. **W1C-PRE-CHAIN-04 — Şema sürümü uyumsuz** `[unit]`
     - Girdi: DB sürümü ikiliden yeni (MIG-05).
     - Beklenen: tipli ret; preflight migration uygulamaz (salt okuma).
     - Kaynak: SPEC#20; savunmacı.

### 10.6 Canlı (LIVE)

125. **W1C-PRE-LIVE-01 — Kullanıcının salt okuma anahtarıyla preflight** `[live]`
     - Girdi: `go run ./cmd/traderd preflight -config configs/example.yaml -credentials ~/.config/trader/binance.json`.
     - Beklenen: saat sapması ölçülür ve yazılır; apiRestrictions alanları (değerler değil, alan adları ve bool değerleri; sır yok) raporlanır; gerçek yanıttaki tüm alan adları test sabitleriyle karşılaştırılır (boşluk 7'yi kapatır); STP kontrolü geçer; çıktı paylaşılabilir.
     - Kaynak: SPEC#18, SPEC#21.
126. **W1C-PRE-LIVE-02 — Demo'da `/sapi` ölçümü** `[live]`
     - Girdi: Demo anahtarıyla preflight.
     - Beklenen: `/sapi/v1/account/apiRestrictions` HTTP kodu ve gövde türü kaydedilir; KEY-14 ya da KEY-16 yolu gözlenir; sonuç `docs/exchanges/binance.md`'ye yazılır (SPEC#6 bilinmeyeni kapanır).
     - Kaynak: SPEC#6; E§D.
127. **W1C-PRE-LIVE-03 — Gerçek exchangeInfo STP alanları** `[live]`
     - Girdi: BTCUSDT, ETHUSDT, SOLUSDT için exchangeInfo.
     - Beklenen: `allowedSelfTradePreventionModes` alanı her sembolde var ve `EXPIRE_TAKER` içerir (BIN ölçümüyle tutarlı); değilse config önerisi güncellenir.
     - Kaynak: BIN (STP ölçümü); REST:Exchange information.

---

## Sayım

Senaryolar 1–127 arası sıralı numaralıdır; her ID benzersizdir.

| Bileşen | Senaryo | unit | fuzz | live |
| --- | --- | --- | --- | --- |
| CFG | 6 | 6 | 0 | 0 |
| DB | 11 | 11 | 0 | 0 |
| MIG | 8 | 8 | 0 | 0 |
| EVT | 13 | 12 | 1 | 0 |
| CHAIN | 15 | 14 | 1 | 0 |
| INT | 8 | 8 | 0 | 0 |
| SCHEMA | 5 | 5 | 0 | 0 |
| CRASH | 6 | 5 | 0 | 1 |
| BAK | 10 | 10 | 0 | 0 |
| PRE-GEN | 6 | 6 | 0 | 0 |
| PRE-CLK | 7 | 7 | 0 | 0 |
| PRE-KEY | 17 | 17 | 0 | 0 |
| PRE-STP | 8 | 8 | 0 | 0 |
| PRE-CHAIN | 4 | 4 | 0 | 0 |
| PRE-LIVE | 3 | 0 | 0 | 3 |
| **Toplam** | **127** | **121** | **2** | **4** |

Not: W1C-CRASH-06 `[live]` olarak işaretlendi çünkü CI'da çalışamaz; Binance gerektirmez, gerçek VM/donanım gerektirir.
