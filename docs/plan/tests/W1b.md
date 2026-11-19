# W1b test senaryoları — WebSocket piyasa verisi ve yerel emir defteri

Hazırlayan: bağımsız test tasarımcısı (koda bakılmadan, yalnız spesifikasyon ve resmi dokümandan). Tarih: 2026-11-19.

## Kaynak kısaltmaları

| Kısaltma | Dosya / bölüm |
| --- | --- |
| `SPEC#n` | `docs/plan/01-waves.md`, W1b tablosu, satır `n` (ör. `SPEC#3(b)` = Kapsam, madde b) |
| `E-n` | `docs/plan/01-waves.md`, "Emir yolu tasarım kuralları" |
| `WSS:<bölüm>` | `/tmp/bdocs/web-socket-streams.md` (GH/web-socket-streams.md) |
| `REST:<bölüm>` | `/tmp/bdocs/rest-api.md` |
| `ENUM`, `FILT`, `ERR`, `CL` | `enums.md`, `filters.md`, `errors.md`, `CHANGELOG.md` (aynı klasör) |
| `E§A` | `research/E_binance_impl_facts.md` bölüm A |
| `ADR-004` | `docs/adr/004-strict-config.md` |

Tür etiketleri: `[unit]` CI'da çalışır (sahte WS sunucusu `httptest` + sahte saat + sahte REST); `[fuzz]` Go fuzz hedefi (CI'da kısa seed korpusu, uzun koşu ayrı); `[live]` gerçek Binance erişimi gerektirir, `//go:build live`, CI dışında kullanıcı makinesinde/VPS'te.

Ortak test sabitleri: `T0 = 1795132800000` (2026-11-20 00:00:00.000 UTC, Cuma). BTCUSDT `tickSize=0.01000000`, `stepSize=0.00001000` (docs/exchanges/binance.md ölçümü). `max_data_age_ms=5000`, `reconnect_max_backoff_s=60`, `depth_limit=1000` (aksi belirtilmedikçe).

## Spesifikasyon ile doküman arasındaki gerilimler (önce okunmalı)

Bu maddeler test beklentisini etkiler; orkestratör spesifikasyonu ya da testi buna göre netleştirmeli.

1. **Güncelleme kuralında `u == localID`.** Doküman canlı olay için yalnız `u < localID` ise yok say diyor (WSS: "How to manage…", update procedure 1). Spesifikasyon `u <= lastUpdateId` atılır diyor; bu doküman adım 5'e (tampon temizliği) karşılık gelir. Canlı akışta `u == localID` olan olay belgelenmemiştir. Savunmacı beklenti: atılır (son uygulanan olayın kopyasıdır; set semantiği nedeniyle yeniden uygulamak da zararsız, ama atmak örtüşen bağlantı kopyalarını sayaçta görünür kılar). Bkz. W1B-BOOK-11.
2. **`serverShutdown` öncesi süre.** E§A "10 dakika önce" diyor; bu bilgi eskidir. CL 2026-06-09: "All mention of a fixed time before the server shuts down has been removed." Testler sabit süre varsaymaz; `T` alanı (CL 2026-11-11) kullanılır, `T` yoksa ya da geçmişteyse hemen devredilir.
3. **24 saat sınırı yalnız `stream.binance.com` için yazılı.** WSS: "A single connection to stream.binance.com is only valid for 24 hours". Spesifikasyonun hostu `data-stream.binance.vision`; bu host için süre belgelenmemiştir. Savunmacı beklenti: aynı 23 sa 50 dk planlı devir uygulanır.
4. **Bağlantı limiti metni.** WSS: "300 connections per attempt every 5 minutes per IP" (cümle bozuk); E§A bunu "300 connection attempts per 5 minutes" diye okur. Spesifikasyon bütçesi 150/5 dk; testler başarısız denemeleri de sayar (muhafazakâr okuma).
5. **Ağırlık havuzu host bazında mı IP bazında mı.** REST: "The limits on the API are based on the IPs, not the API keys." `data-api.binance.vision` ile `api.binance.com` aynı IP havuzunu paylaşıyor mu belgelenmemiş. Spesifikasyon host bazlı sayaç istiyor. `%20` emir rezervi yalnız imzalı host sayacında anlamlıdır; paylaşım varsa piyasa verisi trafiği rezervi yiyebilir. Bkz. W1B-REST-13 (live ölçüm).
6. **Pong ve giden sınırlayıcı.** WSS Limits: sunucuya gelen 5 mesaj/sn sınırına PING ve PONG çerçeveleri dahil. Spesifikasyon giden ≤ 4/sn diyor. Pong'un sınırlayıcıdan geçip geçmediği yazılmamış; savunmacı beklenti: geçer, ama sınırlayıcı kuyruğunda öncelikli ve aç kalmaz (bkz. W1B-LIM-02).
7. **`int64` taşmasında "ölçek büyütülerek yeniden kur".** Ölçeği büyütmek taşmayı düzeltmez, kötüleştirir. Spesifikasyon "aynı yola gider" diyor. Savunmacı beklenti: yeniden kurma + olay; ikinci denemede de taşarsa sonsuz döngü yok, sembol `Stale` ve tipli hata (bkz. W1B-FIX-12).
8. **`TickSize` nil iken "en büyük ondalık basamak sayısı".** Sondaki sıfırların sayılıp sayılmadığı belirsiz (`"4.00000000"` 8 mi 0 mı). Snapshot dizgileri Binance'te sabit genişlikte gelir (REST: depth örneği). Savunmacı beklenti: yazılı basamak sayısı (sondaki sıfırlar dahil) kullanılır; her iki yorum da test vektöründe sabitlenmeli (bkz. W1B-FIX-06).
9. **Bilinmeyen olay → bağlantı yenileme.** Spesifikasyon bilinmeyen olayda bağlantıyı yeniliyor. Binance yeni alan ekleyebilir; bilinmeyen *alan* yenileme sebebi olmamalı, yalnız bilinmeyen *akış adı / olay tipi* olmalı (bkz. W1B-PARSE-10/11).

---

## 1. Config ve komut satırı (CFG)

1. **W1B-CFG-01 — Geçerli `marketdata` bloğu** `[unit]`
   - Ön koşul: `configs/example.yaml` + `marketdata: {depth_limit: 1000, kline_intervals: [1m, 1h], reconnect_max_backoff_s: 60}`.
   - Girdi: `traderd validate -config f`.
   - Beklenen: çıkış kodu 0; config nesnesinde üç alan aynen.
   - Kaynak: SPEC#14.
2. **W1B-CFG-02 — `depth_limit` sınırları** `[unit]`
   - Girdi: tablo `0, 1, 100, 101, 500, 501, 1000, 1001, 5000, 5001, -1, 1000.0, "1000", 1e3`.
   - Beklenen: `1..5000` tam sayılar kabul; `0, 5001, -1` alan adıyla ret; `1000.0` ve `1e3` (ondalık/üstel) ret (ondalık→tam sayı kesilmez); `"1000"` (dizgi) ret.
   - Kaynak: SPEC#14; ADR-004 ("Tam sayı alanlarına ondalıklı değer yazılamaz"); REST:Order book (limit en fazla 5000).
3. **W1B-CFG-03 — `kline_intervals` beyaz listesi** `[unit]`
   - Girdi: tek tek `1s 1m 3m 5m 15m 30m 1h 2h 4h 6h 8h 12h 1d 3d 1w 1M` → kabul; `1H`, `60m`, `2d`, `1y`, `""`, `1m@+08:00`, boş liste `[]`, tekrar `[1m, 1m]`.
   - Beklenen: 16 değerin hepsi kabul. Büyük/küçük harf duyarlı: `1M` (ay) ≠ `1m` (dakika), ikisi birlikte kabul ve ayrı akış. `1H`, `60m`, `2d`, `1y`, boş dizgi, saat dilimli ad ret. Boş liste ret (kline akışı zorunlu; spesifikasyon "zorunlu" diyor). Tekrar: savunmacı ret (spesifikasyon sessiz).
   - Kaynak: SPEC#14; WSS:Kline intervals; REST:Kline ("case-sensitive").
4. **W1B-CFG-04 — `reconnect_max_backoff_s` sınırları** `[unit]`
   - Girdi: `0, 1, 300, 301, 1.5, -5`.
   - Beklenen: `1` ve `300` kabul; diğerleri ret.
   - Kaynak: SPEC#14; ADR-004.
5. **W1B-CFG-05 — Eksik / bilinmeyen anahtar** `[unit]`
   - Girdi: (a) `marketdata` bloğu yok; (b) `depth_limit` eksik; (c) `marketdata.depthlimit` (yazım hatası).
   - Beklenen: üçü de ret, hata mesajı anahtar yolunu içerir; varsayılan değer uygulanmaz.
   - Kaynak: SPEC#14 ("Zorunlu"); configs/example.yaml başlığı ("loader rejects unknown keys and has no defaults").
6. **W1B-CFG-06 — Şema senkronu** `[unit]`
   - Girdi: `configs/schema.json` ve `configs/example.yaml`.
   - Beklenen: şema yeni üç alanı aynı sınırlarla içerir; örnek config şemaya karşı geçerli; mevcut CI şema senkron kontrolü geçer.
   - Kaynak: SPEC#10, SPEC#11 ("yalnız ekleme").
7. **W1B-CFG-07 — Eski komut biçimi** `[unit]`
   - Girdi: `traderd -config configs/example.yaml` (alt komutsuz).
   - Beklenen: `traderd validate -config …` ile bayt bayt aynı stdout/stderr ve çıkış kodu.
   - Kaynak: SPEC#3(g).
8. **W1B-CFG-08 — Bilinmeyen alt komut ve eksik bayrak** `[unit]`
   - Girdi: `traderd foo`, `traderd observe` (config yok), `traderd observe -config f` (symbols yok), `traderd observe -config f -symbols ""`.
   - Beklenen: sıfırdan farklı çıkış kodu, kullanım mesajı; ağ bağlantısı açılmaz (sahte dialer'a çağrı 0).
   - Kaynak: SPEC#3(g), SPEC#12.
9. **W1B-CFG-09 — Sembol listesi normalizasyonu** `[unit]`
   - Girdi: `-symbols BTCUSDT,ethusdt, SOLUSDT,BTCUSDT`.
   - Beklenen: savunmacı: boşluk kırpılır, tekrar tekilleştirilir, akış adlarında küçük harf (`btcusdt@bookTicker`), olaylarda ve çıktıda büyük harf `BTCUSDT`. Geçersiz karakter (`BTC/USDT`, `BTC USDT`, ASCII dışı) ret.
   - Kaynak: WSS:General ("All symbols for streams are lowercase"); spesifikasyon normalizasyonu tanımlamıyor (savunmacı).
10. **W1B-CFG-10 — `-duration` ve `-out` doğrulaması** `[unit]`
    - Girdi: `-duration 0`, `-duration -1h`, `-duration abc`, `-out /root/yok/observe.jsonl` (yazılamaz), `-out` mevcut dosya.
    - Beklenen: ilk üçü ret; yazılamayan yol bağlantı açılmadan ret; mevcut dosya: savunmacı ret ya da açık `-force` (spesifikasyon sessiz; üzerine sessizce yazılmaz).
    - Kaynak: SPEC#12 (çalışma yüzeyi); spesifikasyon sessiz (savunmacı).

## 2. WebSocket bağlantısı, ping/pong, okuma zaman aşımı (CONN)

11. **W1B-CONN-01 — URL biçimi** `[unit]`
    - Ön koşul: semboller `BTCUSDT, ETHUSDT`, aralıklar `1m, 1h`.
    - Girdi: istemci bağlanır; sahte sunucu istek yolunu kaydeder.
    - Beklenen: host `data-stream.binance.vision` (test sunucusunda base URL enjekte edilir), yol `/stream`, sorgu `streams=` içinde tam 10 akış: her sembol için `@kline_1m`, `@kline_1h`, `@bookTicker`, `@depth@100ms`, `@aggTrade`; hepsi küçük harf, `/` ile ayrılmış; `timeUnit` parametresi yok (milisaniye varsayılanı).
    - Kaynak: SPEC#3(a); WSS:General (combined format, lowercase, ms default).
12. **W1B-CONN-02 — Combined zarf ayrıştırma** `[unit]`
    - Girdi: `{"stream":"btcusdt@bookTicker","data":{"u":400900217,"s":"BTCUSDT","b":"25.35190000","B":"31.21000000","a":"25.36520000","A":"40.66000000"}}`.
    - Beklenen: book ticker olayı BTCUSDT'ye yönlenir; `u=400900217`.
    - Kaynak: WSS:General, WSS:Individual Symbol Book Ticker.
13. **W1B-CONN-03 — Ping'e aynı payload ile pong** `[unit]`
    - Girdi: sunucu ping gönderir, payload sırasıyla `""`, `"abc"`, 125 baytlık rastgele dizi (RFC 6455 kontrol çerçevesi üst sınırı).
    - Beklenen: her ping için tam aynı payload'lı pong; sunucu tarafında gözlenen gecikme < 1 sn (sahte saatte 0 ilerleme).
    - Kaynak: SPEC#3(a); WSS:General ("send a pong with a copy of ping's payload as soon as possible").
14. **W1B-CONN-04 — Yavaş tüketici pong'u bloklamaz** `[unit]`
    - Ön koşul: aşağı akış tüketicisi (defter) 10 sn boyunca okumayı durdurur.
    - Girdi: sunucu bu sürede saniyede 50 depth olayı + 20 sn'de bir ping.
    - Beklenen: pong'lar zamanında gider (okuma goroutine'i aşağı akış kanalında bloklanmaz). Tampon dolarsa: sınırlı kuyruk, taşmada sembol `Stale` + defter yeniden kurma isteği ya da bağlantı yenileme; bellek sınırsız büyümez. Davranış tipli olayla günlüğe yazılır.
    - Kaynak: WSS:General (1 dk içinde pong yoksa kopar); spesifikasyon geri basıncı tanımlamıyor (savunmacı).
15. **W1B-CONN-05 — İstenmemiş pong gönderilmez / boş gönderilir** `[unit]`
    - Girdi: 3 dk boyunca normal akış.
    - Beklenen: istemci kendiliğinden pong göndermiyorsa sayaç 0; gönderiyorsa payload boş. Hiçbir durumda istenmemiş pong canlılık mekanizması olarak kullanılmaz.
    - Kaynak: WSS:General ("Unsolicited pong frames are allowed, but will not prevent disconnection… recommended that the payload… are empty").
16. **W1B-CONN-06 — 60 sn sessizlik → yeniden bağlanma** `[unit]`
    - Ön koşul: sahte saat; sunucu bağlantıyı açık tutar ama hiçbir çerçeve (veri ya da ping) göndermez.
    - Girdi: saat 59.999 sn ilerletilir, sonra 60.001 sn'ye.
    - Beklenen: 59.999'da bağlantı açık; 60.001'de bağlantı kapatılır, `reconnect` olayı, yeni dial denemesi (geri çekilme kuralına göre). Etkilenen semboller `Stale`.
    - Kaynak: SPEC#3(a) ("okuma zaman aşımı 60 sn"), SPEC#15.
17. **W1B-CONN-07 — Ping çerçevesi okuma zamanlayıcısını sıfırlar** `[unit]`
    - Girdi: veri yok, yalnız 20 sn'de bir ping; 5 dk.
    - Beklenen: zaman aşımı tetiklenmez (ping de gelen trafiktir). Ancak tazelik (FRESH) bu sürede sembolleri `Stale` raporlar: ping piyasa verisi değildir.
    - Kaynak: WSS:General (sunucu 20 sn'de bir ping); SPEC#3(e). Ping'in zamanlayıcıyı sıfırlaması spesifikasyonda yazılı değil; savunmacı beklenti budur, aksi halde sessiz piyasada gereksiz yeniden bağlanma olur.
18. **W1B-CONN-08 — Sunucu bağlantıyı kapatır (close frame, kod 1000/1001/1006)** `[unit]`
    - Girdi: sunucu (a) normal close 1000, (b) 1001 going away, (c) TCP reset (close frame yok).
    - Beklenen: üçünde de yeniden bağlanma; (c) için okuma hatası tipli; panik yok; defter boşluk kontrolüne girer (BOOK-20).
    - Kaynak: SPEC#3(a); WSS:General (24 sa'te kopma beklenir).
19. **W1B-CONN-09 — Dial zaman aşımı ve TLS/HTTP hatası** `[unit]`
    - Girdi: (a) sunucu TCP kabul eder, upgrade yanıtı vermez; (b) HTTP 403; (c) HTTP 451; (d) HTTP 429; (e) DNS hatası.
    - Beklenen: dial her durumda context'e bağlı zaman aşımıyla biter (sınırsız bekleme yok); her deneme bağlantı bütçesinden düşer (RECON-06); hata tipli; geri çekilmeli yeniden deneme. (c) 451 coğrafi engel: savunmacı, açık hata mesajı ("erişim engeli") ve yine sınırlı yeniden deneme.
    - Kaynak: SPEC#3(a); 00-overview "her ağ çağrısı context alır, zaman aşımı vardır"; 451 davranışı WS için belgelenmemiş (ÖLÇÜM-2 REST için gözlemledi).
20. **W1B-CONN-10 — Upgrade sonrası yönlendirme / http şeması reddi** `[unit]`
    - Girdi: base URL `ws://` (şifresiz) ya da sunucu 301 yönlendirme.
    - Beklenen: savunmacı: `wss` dışı şema config/derleme düzeyinde ret; yönlendirme izlenmez (mevcut REST istemcisindeki "https zorunlu, yönlendirme reddi" ile tutarlı).
    - Kaynak: 01-waves §1 (mevcut REST kuralları); WS için belgelenmemiş.
21. **W1B-CONN-11 — 1024 akış sınırı ve bağlantı bölme** `[unit]`
    - Ön koşul: 2 kline aralığı → sembol başına 5 akış.
    - Girdi: 204 sembol (1020 akış), 205 sembol (1025 akış).
    - Beklenen: 204 → tek bağlantı; 205 → iki bağlantı, hiçbiri 1024'ü aşmaz; her akış tam bir bağlantıda. URL uzunluk sınırı belgelenmemiş: savunmacı, bağlantı başına akış sayısı config'lenebilir alt sınırla bölünür ve test sunucusu 8 KB üstü URL'yi reddettiğinde bölme devreye girer.
    - Kaynak: WSS:Limits ("maximum of 1024 streams"); URL sınırı belgelenmemiş.
22. **W1B-CONN-12 — Canlı ping/pong ve bağlantı süresi** `[live]`
    - Girdi: `data-stream.binance.vision` ile 30 dk bağlantı.
    - Beklenen: ping aralığı medyanı ≈ 20 sn (kayıt edilir); kopma yok; pong'lar kabul edilir.
    - Kaynak: WSS:General; SPEC#6 ("observe ile ölçülür").

## 3. Bağlantı devri: planlı ve `serverShutdown` (ROT)

23. **W1B-ROT-01 — Planlı devir 23:50:00'da** `[unit]`
    - Ön koşul: sahte monotonik saat; bağlantı A `t=0`'da açıldı.
    - Girdi: saat `23:49:59.999` → `23:50:00.000`.
    - Beklenen: ilkinde yeni dial yok; ikincisinde bağlantı B açılır. B ilk veri mesajını aldıktan sonra A kapatılır. Devir boyunca tüketiciye kesintisiz akış (FRESH stale olmaz, `max_data_age_ms=5000` içinde). `rotation{reason:"planned"}` olayı.
    - Kaynak: SPEC#3(a) ("23 sa 50 dk'da planlı devir"), SPEC#15 ("24 sa devri (sahte saat)").
24. **W1B-ROT-02 — Planlı devir duvar saatine değil monotonik saate bağlı** `[unit]`
    - Girdi: bağlantı açıkken duvar saati +2 sa ileri (NTP adımı), sonra −2 sa geri.
    - Beklenen: devir zamanı değişmez (A açıldıktan 23 sa 50 dk sonra, monotonik).
    - Kaynak: SPEC#3(a); saat edge case (savunmacı, spesifikasyon saat kaynağını yazmıyor).
25. **W1B-ROT-03 — Devirde yeni bağlantı açılamaz** `[unit]`
    - Girdi: 23:50'de B dial'i 3 kez başarısız (HTTP 503), A çalışmaya devam ediyor; 24:00'da sunucu A'yı kapatır.
    - Beklenen: A, B hazır olana kadar kapatılmaz; B denemeleri geri çekilme ve bütçeye tabi; 24:00'te A düşünce normal yeniden bağlanma; defter boşluk kontrolü (gerekirse yeniden kurma), kline boşluk kontrolü (gerekirse REST).
    - Kaynak: SPEC#3(a); WSS:General ("expect to be disconnected at the 24 hour mark").
26. **W1B-ROT-04 — `serverShutdown` (combined) → hemen yeni bağlantı** `[unit]`
    - Girdi: A üzerinden `{"stream":"!serverShutdown","data":{"e":"serverShutdown","E":1795132800000,"T":1795133400000}}` (T = E + 10 dk).
    - Beklenen: B hemen (geri çekilme beklemeden, ama bağlantı bütçesine tabi) açılır; B veri vermeye başlayınca A kapatılır; A en geç `T`'de kapatılmış olur. `rotation{reason:"serverShutdown", T:…}` olayı. Bu olay "bilinmeyen olay" sayılmaz (PARSE-10 ile karıştırılmaz).
    - Kaynak: SPEC#3(a); WSS:Server Shutdown; CL 2026-11-11 (`T` alanı).
27. **W1B-ROT-05 — `serverShutdown` raw biçimi** `[unit]`
    - Girdi: combined bağlantıda zarfsız `{"e":"serverShutdown","E":1795132800000,"T":1795133400000}`.
    - Beklenen: savunmacı: aynı devir davranışı (doküman iki biçimi de gösteriyor; combined bağlantıda hangisinin geleceği yalnız örnekle belgelenmiş).
    - Kaynak: WSS:Server Shutdown (raw ve combined örnekleri).
28. **W1B-ROT-06 — `serverShutdown`'da `T` yok, geçmişte ya da E'den küçük** `[unit]`
    - Girdi: (a) `T` alanı yok; (b) `T=E−1000`; (c) `T=0`; (d) `T` yerel saatten 1 sa ileride.
    - Beklenen: (a)–(c) devir hemen, A, B hazır olunca kapatılır (beklenecek süre yok); (d) devir yine hemen başlar, A'nın kapanışı B hazır olunca, `T` beklenmez. Hiçbirinde sabit "10 dk" varsayımı yok.
    - Kaynak: CL 2026-06-09 (sabit süre kaldırıldı); CL 2026-11-11 (`T` yeni alan; eski sunucuda olmayabilir — belgelenmemiş).
29. **W1B-ROT-07 — Arka arkaya iki `serverShutdown`** `[unit]`
    - Girdi: A'da `serverShutdown`; B açılırken A'da ikinci `serverShutdown`; B açıldıktan 1 sn sonra B'de de `serverShutdown`.
    - Beklenen: A için tek devir (ikinci olay yinelenen devir başlatmaz); B'nin kendi devri C'yi açar; aynı anda en fazla 2 bağlantı (eski + yeni) aynı akış kümesi için.
    - Kaynak: SPEC#3(a); belgelenmemiş sıklık (SPEC#6 "observe ile ölçülür"); savunmacı.
30. **W1B-ROT-08 — Örtüşmede yinelenen depth olayları** `[unit]`
    - Ön koşul: defter `localID=1000`.
    - Girdi: A: `U=1001,u=1005`; B (yeni): `U=1001,u=1005` (aynı olay), sonra A: `U=1006,u=1010`, B: `U=1006,u=1010`, B: `U=1011,u=1012`; A kapanır.
    - Beklenen: her olay defterde bir kez uygulanır (`localID` sırasıyla 1005, 1010, 1012); yinelenenler `u <= localID` kuralıyla atılır; yeniden kurma yok; `duplicate_dropped` sayacı = 2.
    - Kaynak: WSS:How to manage (update procedure 1); SPEC#3(b).
31. **W1B-ROT-09 — Örtüşmede sıra dışı depth (yeni bağlantı geride)** `[unit]`
    - Ön koşul: `localID=1010` (A'dan).
    - Girdi: B'nin ilk olayı `U=1006,u=1010`, ardından `U=1011,u=1015`.
    - Beklenen: ilk olay atılır (eski), ikinci uygulanır; yeniden kurma yok.
    - Kaynak: WSS:How to manage.
32. **W1B-ROT-10 — Örtüşmede yeni bağlantı ileride, boşluklu** `[unit]`
    - Ön koşul: `localID=1010`; A artık mesaj göndermiyor.
    - Girdi: B'nin ilk olayı `U=1020,u=1025`.
    - Beklenen: `U > localID+1` → boşluk; defter atılır ve yeniden kurulur (BOOK yeniden kurma yolu); A'dan gecikmeli `U=1011,u=1019` gelse bile defter yeniden kurulumdan sonra tutarlıdır (yeniden kurma sırasında A'nın olayları tampona girmez ya da `u` kuralıyla atılır). Savunmacı alternatif: A'nın gecikmeli olayı gelene kadar kısa bekleme yapılmaz (belgelenmemiş; basit ve doğru yol yeniden kurmadır).
    - Kaynak: WSS:How to manage ("If the event first update ID (U) is greater than… you have missed some events").
33. **W1B-ROT-11 — Örtüşmede yinelenen kapanmış mum** `[unit]`
    - Girdi: A ve B aynı `kline_1m` `t=T0, T=T0+59999, x=true` olayını taşır.
    - Beklenen: `ClosedKline` bir kez yayınlanır (anahtar: sembol + aralık + `t`).
    - Kaynak: SPEC#3(c); REST:Kline ("Klines are uniquely identified by their open time").
34. **W1B-ROT-12 — Örtüşmede yinelenen aggTrade** `[unit]`
    - Girdi: A: `a=12345, a=12346`; B: `a=12345, a=12346, a=12347`.
    - Beklenen: `Trade` yayını `12345, 12346, 12347`, her biri bir kez, artan sırada.
    - Kaynak: SPEC#3(d); WSS:Aggregate Trade (`a` aggregate trade ID).
35. **W1B-ROT-13 — Örtüşmede bookTicker geri gitmez** `[unit]`
    - Girdi: A: `u=500`; B: `u=498`, sonra `u=501`.
    - Beklenen: en iyi fiyat `u=498` ile geri sarılmaz; `u=501` uygulanır. `u`'nun bookTicker'da monoton olduğu belgelenmemiş: savunmacı, `u` küçükse at.
    - Kaynak: WSS:Book Ticker (`u` "Order book updateId"); monotonluk belgelenmemiş.
36. **W1B-ROT-14 — Canlı devir gözlemi** `[live]`
    - Girdi: 24 sa observe.
    - Beklenen: planlı devir en az bir kez; varsa `serverShutdown` olayları ve `T−E` süreleri raporlanır; devir kaynaklı kapanmış mum kaybı 0.
    - Kaynak: SPEC#6, SPEC#21.

## 4. Yeniden bağlanma geri çekilmesi ve bağlantı bütçesi (RECON)

37. **W1B-RECON-01 — Üstel artış ve tavan** `[unit]`
    - Ön koşul: jitter kapalı (test kancası) ya da deterministik jitter kaynağı; `max=60`.
    - Girdi: art arda 10 başarısız dial.
    - Beklenen: taban gecikmeler `1, 2, 4, 8, 16, 32, 60, 60, 60, 60` sn.
    - Kaynak: SPEC#3(a) ("1 sn → reconnect_max_backoff_s").
38. **W1B-RECON-02 — Jitter sınırları** `[unit]`
    - Girdi: rastgele kaynakla 10 000 örnek, her deneme numarası n=1..12.
    - Beklenen: her gecikme `0 < d ≤ min(2^(n-1), max)` sn ve hiçbir zaman tavanı aşmaz; jitter formülü dokümante edilir ve test o formülün alt sınırını da doğrular. Aynı anda başlayan iki bağlantının gecikmeleri birebir aynı değildir.
    - Kaynak: SPEC#3(a) ("jitter"; formül tanımlı değil).
39. **W1B-RECON-03 — `max=1` ve `max=300` kenarları** `[unit]`
    - Beklenen: `max=1` → tüm gecikmeler ≤ 1 sn; `max=300` → 9. denemede 256, 10.'da 300; tamsayı taşması yok (n=1000 denemede bile).
    - Kaynak: SPEC#14 (1..300).
40. **W1B-RECON-04 — Başarılı bağlantı sonrası sıfırlama** `[unit]`
    - Girdi: 5 başarısız dial (gecikme 16 sn'ye ulaştı), sonra başarılı bağlantı 10 dk veri aldı, sonra kopma.
    - Beklenen: sonraki gecikme yeniden 1 sn tabanından başlar. Bağlanıp hiç veri gelmeden hemen kopan bağlantı (ör. 0.5 sn) sıfırlama sayılmaz (savunmacı; aksi halde hızlı döngü bütçeyi tüketir).
    - Kaynak: spesifikasyon sıfırlama koşulunu tanımlamıyor (savunmacı).
41. **W1B-RECON-05 — `serverShutdown` devri geri çekilmeye girmez** `[unit]`
    - Girdi: geri çekilme seviyesi 32 sn iken `serverShutdown` gelir.
    - Beklenen: B hemen açılır (geri çekilme gecikmesi uygulanmaz) ama bağlantı bütçesinden düşer.
    - Kaynak: WSS:Server Shutdown ("establish a new connection as soon as possible").
42. **W1B-RECON-06 — Bağlantı denemesi bütçesi: 150 / 5 dk** `[unit]`
    - Ön koşul: sahte saat; her dial anında başarısız; geri çekilme test kancasıyla 0.
    - Girdi: 5 dk içinde 200 dial isteği.
    - Beklenen: kayan pencerede en fazla 150 deneme (başarılı + başarısız); 151. istek pencere açılana kadar bekletilir (dial edilmez); `connect_budget_exhausted` olayı bir kez (her denemede değil) yazılır.
    - Kaynak: SPEC#3(a) ("300/5 dk'nın yarısı"); WSS:Limits.
43. **W1B-RECON-07 — Bütçe kayan pencere sınırı** `[unit]`
    - Girdi: `t=0`'da 150 deneme; `t=299.999 sn`'de 1 istek; `t=300.000 sn`'de 1 istek.
    - Beklenen: ilki bekletilir, ikincisi izin alır (pencere kapsayıcılığı dokümante edilir ve test buna sabitlenir).
    - Kaynak: SPEC#3(a).
44. **W1B-RECON-08 — Bütçe tüm bağlantılarda paylaşılır** `[unit]`
    - Ön koşul: 205 sembol → 2 bağlantı (CONN-11).
    - Girdi: iki bağlantı aynı anda kopma döngüsünde.
    - Beklenen: toplam deneme 150/5 dk'yı aşmaz (bütçe süreç genelinde tek). Süreçler arası paylaşım (ikinci `traderd` aynı IP'de): belgelenmemiş ve uygulanamaz; runbook'ta not.
    - Kaynak: WSS:Limits ("per IP").
45. **W1B-RECON-09 — Geri çekilme beklerken context iptali** `[unit]`
    - Girdi: gecikme 60 sn; 1 sn sonra context iptal.
    - Beklenen: bekleme hemen biter, yeni dial yok, goroutine'ler zaman sınırında (LIFE-01) biter.
    - Kaynak: SPEC#15.

## 5. Giden mesaj sınırlayıcı (LIM)

46. **W1B-LIM-01 — Giden ≤ 4/sn** `[unit]`
    - Girdi: sunucu 1 sn içinde 10 ping gönderir.
    - Beklenen: herhangi bir 1 sn penceresinde istemciden sunucuya en fazla 4 mesaj (pong dahil). Ping'lerin hepsine (ya da en azından en sonuncusuna) 60 sn içinde pong verilir. Sunucu "5/sn aşıldı" diye kesmez.
    - Kaynak: SPEC#3(a) ("giden mesaj sınırlayıcı ≤ 4/sn"); WSS:Limits (PING ve PONG sayılır).
47. **W1B-LIM-02 — Pong önceliği** `[unit]`
    - Ön koşul: istemci (gelecekte) SUBSCRIBE gibi kontrol mesajı kuyruğu dolu (test kancasıyla 20 mesaj).
    - Girdi: kuyruk boşalırken ping gelir.
    - Beklenen: pong kuyruğun önüne geçer; 1 sn içinde gider; toplam yine ≤ 4/sn.
    - Kaynak: WSS:General ("as soon as possible"); spesifikasyon öncelik tanımlamıyor (savunmacı).
48. **W1B-LIM-03 — Kontrol mesajı biçimi (kullanılırsa)** `[unit]`
    - Girdi: istemci SUBSCRIBE/UNSUBSCRIBE gönderirse.
    - Beklenen: `id` 64-bit işaretli tamsayı ya da ≤ 36 alfanümerik; yanıt `{"result":null,"id":N}` başarı; `{"code":2,"msg":…}` hata tipli döner. Spesifikasyon URL tabanlı abonelik kullandığından bu yol yoksa test "N/A" olarak işaretlenir.
    - Kaynak: WSS:Live Subscribing, WSS:Error Messages.

## 6. Ayrıştırma, bozuk ve büyük mesajlar (PARSE)

49. **W1B-PARSE-01 — Örnek payload'lar (golden)** `[unit]`
    - Girdi: `testdata/*.json` içinde dokümandaki örneklerin aynısı: kline, aggTrade, bookTicker, depthUpdate, serverShutdown (raw ve combined).
    - Beklenen: tüm alanlar doğru tipe ve değere eşlenir; fiyat/miktar dizgileri `big.Rat` olarak birebir (`"0.0010"` → 1/1000).
    - Kaynak: WSS ilgili bölümler; SPEC#9 (`testdata/*.json`).
50. **W1B-PARSE-02 — Geçersiz JSON** `[unit]`
    - Girdi: `{"stream":"btcusdt@depth@100ms","data":{` (kesik), `not json`, `""`, `null`, `[]`.
    - Beklenen: bağlantı yenilenir (SPEC); olay tipli günlüğe yazılır; panik yok; defter bozulmaz (etkilenen sembol için boşluk kontrolü yeni bağlantıda yapılır).
    - Kaynak: SPEC#15 ("bozuk JSON … → bağlantı yenilenir").
51. **W1B-PARSE-03 — Yanlış tipler** `[unit]`
    - Girdi: `"U":"157"` (dizgi), `"u":157.5`, `"p":0.001` (sayı, dizgi değil), `"x":"true"`, `"b":[["0.0024"]]` (eksik miktar), `"b":[["0.0024","10","extra"]]`, `"b":null`.
    - Beklenen: her biri bozuk mesaj → bağlantı yenileme; kısmi uygulama yok (seviye listesinin yarısı uygulanmaz). `"b":[]` (boş liste) geçerlidir.
    - Kaynak: WSS:Diff Depth (alan tipleri örnekte); bozuk mesaj davranışı SPEC#15.
52. **W1B-PARSE-04 — Eksik zorunlu alan** `[unit]`
    - Girdi: depthUpdate'te `U` yok; kline'da `k.x` yok; aggTrade'de `a` yok; bookTicker'da `u` yok; combined zarfta `data` yok.
    - Beklenen: bozuk mesaj → yenileme; sıfır değer varsayımıyla işlenmez (`U=0` gibi).
    - Kaynak: SPEC#15; savunmacı (Go sıfır değer tuzağı).
53. **W1B-PARSE-05 — `int64` dışı kimlikler** `[unit]`
    - Girdi: `"u":9223372036854775808`, `"U":-1`, `"a":1e20`.
    - Beklenen: bozuk mesaj; sessiz sarma (wrap) yok.
    - Kaynak: savunmacı (belgelenmemiş aralık).
54. **W1B-PARSE-06 — `U > u`** `[unit]`
    - Girdi: `{"e":"depthUpdate","U":200,"u":150,…}`.
    - Beklenen: belgelenmemiş; savunmacı: bozuk mesaj → yenileme (ya da en azından defter yeniden kurma), uygulanmaz.
    - Kaynak: WSS:Diff Depth (`U` first, `u` final); ters sıra belgelenmemiş.
55. **W1B-PARSE-07 — Akış adı ile payload sembolü uyuşmuyor** `[unit]`
    - Girdi: `{"stream":"btcusdt@depth@100ms","data":{"e":"depthUpdate","s":"ETHUSDT",…}}`.
    - Beklenen: savunmacı: bozuk mesaj; ETH defterine de BTC defterine de uygulanmaz.
    - Kaynak: belgelenmemiş (savunmacı).
56. **W1B-PARSE-08 — Kline `k.s` ≠ dış `s`, `k.i` ≠ akış aralığı** `[unit]`
    - Girdi: `btcusdt@kline_1m` akışında `k.i:"5m"`.
    - Beklenen: savunmacı: bozuk mesaj, yayın yok.
    - Kaynak: WSS:Kline payload; belgelenmemiş.
57. **W1B-PARSE-09 — Büyük mesaj** `[unit]`
    - Ön koşul: okuma sınırı (ör. 1 MiB; değer kodda sabit ve dokümante).
    - Girdi: sınır − 1 bayt geçerli mesaj; sınır + 1 bayt mesaj; 64 MiB mesaj.
    - Beklenen: ilki işlenir; ikinci ve üçüncü bellek ayırmadan önce reddedilir (kütüphanenin okuma sınırı), bağlantı yenilenir. Binance mesaj üst sınırı belgelenmemiş: sınır, depth@100ms'in tek olayda gözlenen en büyük boyutunun rahat üstünde seçilmeli (live ölçüm, OBS-04).
    - Kaynak: SPEC#15 ("büyük mesaj → bağlantı yenilenir").
58. **W1B-PARSE-10 — Bilinmeyen akış adı / olay tipi** `[unit]`
    - Girdi: `{"stream":"btcusdt@trade","data":{"e":"trade",…}}` (abone olunmamış), `{"stream":"btcusdt@depth@100ms","data":{"e":"fooUpdate",…}}`.
    - Beklenen: bağlantı yenilenir; olay `unknown_event` tipiyle günlüğe yazılır.
    - Kaynak: SPEC#15 ("bilinmeyen olay → bağlantı yenilenir").
59. **W1B-PARSE-11 — Bilinmeyen ek alan** `[unit]`
    - Girdi: dokümandaki depthUpdate örneğine `"pu":155,"newField":"x"` eklenmiş.
    - Beklenen: savunmacı: ek alan yok sayılır, olay normal işlenir, yenileme **yok** (Binance alan ekleyebilir; CL'de `T` alanı sonradan eklendi).
    - Kaynak: CL 2026-11-11 (sonradan alan ekleme örneği); spesifikasyonla gerilim maddesi 9.
60. **W1B-PARSE-12 — İkili (binary) çerçeve, geçersiz UTF-8** `[unit]`
    - Girdi: binary opcode ile JSON; text çerçevede `0xff 0xfe`.
    - Beklenen: belgelenmemiş; savunmacı: bozuk mesaj → yenileme.
    - Kaynak: WSS:General (UTF-8); CL 2026-06-09 ("serverShutdown events in JSON in WebSocket text frames", SBE bağlamında).
61. **W1B-PARSE-13 — Mikrosaniye zaman damgası** `[unit]`
    - Girdi: `"E":1795132800000000` (16 hane).
    - Beklenen: `timeUnit` istenmediği için beklenmez; savunmacı: makul aralık kontrolü (ör. 2017–2100 arası ms) dışında → bozuk mesaj.
    - Kaynak: WSS:General ("milliseconds by default").
62. **W1B-PARSE-14 — Ayrıştırıcı fuzz** `[fuzz]`
    - Girdi: seed korpus = tüm golden dosyalar + PARSE-02..13 girdileri; rastgele mutasyon.
    - Beklenen: panik yok, sonsuz döngü yok, her girdi ya geçerli olay ya tipli hata; ayrıştırılan olay yeniden serileştirildiğinde fiyat/miktar dizgileri anlamca aynı.
    - Kaynak: SPEC#15 ("Fuzz: olay ayrıştırıcı").

## 7. Yerel emir defteri (BOOK)

Kurallar WSS:"How to manage a local order book correctly" bölümündendir; aşağıda `snap(lid)` snapshot `lastUpdateId`'sini, `ev(U,u)` diff olayını gösterir.

63. **W1B-BOOK-01 — Mutlu yol** `[unit]`
    - Girdi: tampon `ev(157,160)`, `ev(161,165)`; `snap(158)` bids `[["4.00","431.00000"]]`, asks `[["4.01","12.00000"]]`; ardından canlı `ev(166,170)`.
    - Beklenen: snapshot ilk olayın `U=157`'sinden küçük değil → kullanılır; `ev(157,160)` atılmaz (`u=160 > 158`) ve `158 ∈ [157;160]`; `localID` 160 → 165 → 170; `BookView.LastUpdateID=170`, `Stale=false`.
    - Kaynak: WSS:How to manage adım 1–7.
64. **W1B-BOOK-02 — Snapshot eski → yeniden al** `[unit]`
    - Girdi: ilk tamponlanan `U=157`; ilk snapshot `snap(150)`, ikinci `snap(162)`.
    - Beklenen: `150 < 157` → snapshot atılır, yeniden istenir (her istek ağırlık bütçesinden 50 düşer); ikinci kullanılır; `ev(157,160)` atılır, `ev(161,165)` uygulanır.
    - Kaynak: WSS:How to manage adım 4; SPEC#15 ("snapshot eski → yenile").
65. **W1B-BOOK-03 — Snapshot eşitliği `lastUpdateId == U−1` ve `== U`** `[unit]`
    - Girdi: (a) ilk `U=157`, `snap(156)`; (b) `snap(157)`.
    - Beklenen: (a) `156 < 157` → yeniden al (doküman "strictly less" diyor; `snap(156)` ardından gelen `ev(157,…)` mantıken bitişik olsa da doküman yeniden almayı söyler, kurala uyulur); (b) kullanılır.
    - Kaynak: WSS:How to manage adım 4 ("strictly less than the U").
66. **W1B-BOOK-04 — Sürekli eski snapshot: sınırlı yeniden deneme** `[unit]`
    - Girdi: sahte REST her seferinde `snap(100)` döner, ilk `U=157`.
    - Beklenen: belgelenmemiş üst sınır; savunmacı: yeniden denemeler geri çekilmeli ve sayılı (ör. 5), ağırlık bütçesini delmez; sonra sembol `Stale` kalır, tipli olay, kuyruktaki diğer sembolleri bloklamaz.
    - Kaynak: WSS:How to manage adım 4 ("go back to step 3", sınırsız); ağırlık REST:Order book; savunmacı.
67. **W1B-BOOK-05 — Tampon temizliği `u <= lastUpdateId`** `[unit]`
    - Girdi: tampon `ev(150,155)`, `ev(156,160)`, `ev(161,170)`; `snap(160)`.
    - Beklenen: ilk iki olay atılır (`u=160 <= 160` dahil); `ev(161,170)` uygulanır; `localID=170`.
    - Kaynak: WSS:How to manage adım 5; SPEC#15 ("`u <= lastUpdateId` atılır").
68. **W1B-BOOK-06 — Tüm tampon snapshot'tan eski** `[unit]`
    - Girdi: tampon `ev(157,160)`; `snap(175)`; sonraki canlı `ev(161,176)`, sonra `ev(177,180)`.
    - Beklenen: tampon tamamen atılır; `ev(161,176)`: `u=176 >= 175`, `U=161 <= 176` → uygulanır (`175 ∈ [161;176]`); `ev(177,180)` uygulanır.
    - Kaynak: WSS:How to manage adım 5–7.
69. **W1B-BOOK-07 — Snapshot sonrası ilk olay boşluklu** `[unit]`
    - Girdi: tampon `ev(157,160)`, `ev(170,175)` (161–169 kayıp); `snap(162)`.
    - Beklenen: `ev(157,160)` atılır; `ev(170,175)`: `U=170 > 162+1` → boşluk → defter atılır, yeniden kurma sıraya girer.
    - Kaynak: WSS:How to manage (update procedure 1).
70. **W1B-BOOK-08 — Canlı akışta boşluk** `[unit]`
    - Ön koşul: `localID=170`.
    - Girdi: `ev(172,175)`.
    - Beklenen: `172 > 171` → `Stale=true`, defter atılır, yeniden kurma; `book_gap{expected:171, got:172}` olayı. Yeniden kurma bitene kadar `BookView` en iyi fiyat döndürmez ya da `Stale=true` ile döndürür (sözleşme sabitlenir).
    - Kaynak: WSS:How to manage; SPEC#15 ("boşluk → yeniden kur").
71. **W1B-BOOK-09 — Bitişiklik `U == localID+1`** `[unit]`
    - Ön koşul: `localID=170`.
    - Girdi: `ev(171,171)` (tek güncelleme).
    - Beklenen: uygulanır; `localID=171`.
    - Kaynak: WSS:How to manage ("Normally, U of the next event is equal to u + 1").
72. **W1B-BOOK-10 — Örtüşen aralık `U < localID+1 <= u`** `[unit]`
    - Ön koşul: `localID=170`.
    - Girdi: `ev(165,175)`.
    - Beklenen: uygulanır (`u >= localID`, `U <= localID+1`); `localID=175`.
    - Kaynak: WSS:How to manage (update procedure kuralları; ne ignore ne restart koşulu).
73. **W1B-BOOK-11 — Canlı akışta `u == localID`** `[unit]`
    - Ön koşul: `localID=170`.
    - Girdi: `ev(168,170)` iki kez.
    - Beklenen: belgelenmemiş (doküman yalnız `u < localID` için ignore diyor). Savunmacı: atılır, `duplicate_dropped` sayacı artar; defter durumu değişmez. Gerilim maddesi 1.
    - Kaynak: WSS:How to manage; SPEC#15.
74. **W1B-BOOK-12 — Canlı akışta `u < localID`** `[unit]`
    - Ön koşul: `localID=170`.
    - Girdi: `ev(150,160)` ve fiyat seviyesi `["4.00","0"]`.
    - Beklenen: yok sayılır; `4.00` seviyesi silinmez.
    - Kaynak: WSS:How to manage (update procedure 1).
75. **W1B-BOOK-13 — Sıfır miktar siler, yeni seviye ekler, var olanı günceller** `[unit]`
    - Ön koşul: bids `4.00→431`, `3.99→10`.
    - Girdi: `ev(171,171)` b: `[["4.00","0.00000000"],["3.99","5"],["3.98","7"]]`.
    - Beklenen: `4.00` silindi, `3.99=5`, `3.98=7` eklendi; en iyi alış `3.99`.
    - Kaynak: WSS:How to manage (update procedure 2); SPEC#15 ("sıfır miktar silme").
76. **W1B-BOOK-14 — Olmayan seviyeye sıfır miktar** `[unit]`
    - Girdi: `["3.50","0"]` (defterde yok).
    - Beklenen: belgelenmemiş; savunmacı: no-op, hata yok, yeniden kurma yok (snapshot dışı seviyeler bilinmez; WSS notu).
    - Kaynak: WSS:How to manage NOTE (5000 seviye dışı bilinmez).
77. **W1B-BOOK-15 — Aynı olayda aynı fiyat iki kez** `[unit]`
    - Girdi: b: `[["4.00","1"],["4.00","2"]]`.
    - Beklenen: belgelenmemiş; savunmacı: sırayla uygulanır, son değer `2` kalır.
    - Kaynak: WSS (sıralı "for each price level"); belgelenmemiş.
78. **W1B-BOOK-16 — Çapraz defter → yeniden kur** `[unit]`
    - Ön koşul: en iyi alış `4.00`, en iyi satış `4.01`.
    - Girdi: `ev(171,171)` b: `[["4.02","1"]]` (satış tarafında 4.01 silinmeden).
    - Beklenen: olay tamamen uygulandıktan sonra `bestBid (4.02) >= bestAsk (4.01)` → defter atılır, yeniden kurma, `book_crossed` olayı. Eşitlik (`bid == ask`) de çapraz sayılır (savunmacı; spesifikasyon "çapraz" diyor, eşitlik tanımsız). Kontrol olay içinde seviye seviye değil olay sonunda yapılır (aynı olayda ask tarafı da güncelleniyorsa yanlış alarm olmaz).
    - Kaynak: SPEC#15 ("çapraz defter → yeniden kur").
79. **W1B-BOOK-17 — Bir taraf boş** `[unit]`
    - Girdi: tüm satış seviyeleri sıfır miktarla silinir.
    - Beklenen: çapraz değil; `BookView` en iyi satış "yok" döner (sıfır fiyat değil); tüketici bunu ayırt edebilir. Savunmacı: boş taraf tazelik/uyarı olayı üretir.
    - Kaynak: belgelenmemiş (savunmacı).
80. **W1B-BOOK-18 — Snapshot isteği parametresi ve ağırlığı** `[unit]`
    - Girdi: `depth_limit` = 1, 100, 101, 500, 501, 1000, 1001, 5000.
    - Beklenen: istek `GET /api/v3/depth?symbol=BTCUSDT&limit=<n>` piyasa hostuna (`data-api.binance.vision`); ağırlık rezervi sırasıyla 5, 5, 25, 25, 50, 50, 250, 250.
    - Kaynak: SPEC#2, SPEC#3(b); REST:Order book (Weight tablosu).
81. **W1B-BOOK-19 — Snapshot hata yanıtları** `[unit]`
    - Girdi: depth isteği (a) 429 `Retry-After: 7`; (b) 418 `Retry-After: 120`; (c) 500; (d) `{"code":-1121,"msg":"Invalid symbol."}` HTTP 400; (e) gövde `{"lastUpdateId":0,"bids":[],"asks":[]}`; (f) gövde `null`.
    - Beklenen: (a)(b) piyasa hostu geri çekilmesi (REST-04), sembol `Stale`, yeniden kurma ertelenir; (c) sınırlı yeniden deneme; (d) sembol kalıcı hata (yeniden deneme döngüsü yok), tipli olay; (e) belgelenmemiş — savunmacı: boş defter kabul edilmez ya da `Stale` işaretli kabul (sözleşme sabitlenir); (f) ret (mevcut "null gövde reddi").
    - Kaynak: REST:IP Limits, REST:HTTP Return Codes; 01-waves §1 (null gövde reddi).
82. **W1B-BOOK-20 — Yeniden bağlanma sonrası defter sürekliliği** `[unit]`
    - Ön koşul: `localID=170`; bağlantı koptu, 3 sn sonra yeni bağlantı.
    - Girdi: yeni bağlantının ilk olayı (a) `ev(171,180)`; (b) `ev(190,195)`.
    - Beklenen: (a) defter korunur, uygulanır, yeniden kurma yok; (b) boşluk → yeniden kurma. Kopukluk süresince `Stale=true`.
    - Kaynak: WSS:How to manage (update procedure); savunmacı (snapshot zorunlu değil, kural yeterli).
83. **W1B-BOOK-21 — Eşzamanlı yeniden kurma sınırı = 2** `[unit]`
    - Ön koşul: 5 sembol; sahte REST depth yanıtını kontrol edilen kanalla bekletir.
    - Girdi: 5 sembolde aynı anda boşluk.
    - Beklenen: herhangi bir anda en fazla 2 snapshot isteği uçuşta; kalan 3 sıralı kuyrukta; FIFO (ya da dokümante edilen sıra); hepsi sonunda kurulur.
    - Kaynak: SPEC#3(b) ("aynı anda en fazla 2"), SPEC#15.
84. **W1B-BOOK-22 — Aynı sembol kuyruğa iki kez girmez** `[unit]`
    - Girdi: BTCUSDT kuyruktayken iki boşluk daha ve bir çapraz defter.
    - Beklenen: tek kuyruk girişi; tek snapshot.
    - Kaynak: SPEC#3(b) ("sembol başına sıraya konur").
85. **W1B-BOOK-23 — Yeniden kurma sırasında tampon** `[unit]`
    - Girdi: yeniden kurma kuyruğunda 4 sn beklerken 40 olay gelir.
    - Beklenen: olaylar tamponlanır (ilk `U` not edilir); tampon sınırlı (sınır dokümante); taşarsa tampon sıfırlanır ve ilk `U` yeniden belirlenir, bellek sınırsız büyümez.
    - Kaynak: WSS:How to manage adım 2; sınır belgelenmemiş (savunmacı).
86. **W1B-BOOK-24 — `BookView` sözleşmesi** `[unit]`
    - Girdi: bids `4.00/431`, `3.99/10`; asks `4.01/12`, `4.02/1`; `N=1,2,5`.
    - Beklenen: en iyi alış `4.00`, satış `4.01`; N seviye azalan alış / artan satış; `N=5` mevcut 2 seviyeyi döner; `LastUpdateID` ve `Stale` doğru; dönen `*big.Rat` değerleri çağıranın değiştirmesiyle defteri bozmaz (kopya).
    - Kaynak: SPEC#13.
87. **W1B-BOOK-25 — Eşzamanlı okuma ve yazma** `[unit]`
    - Girdi: `-race` altında 8 okuyucu goroutine `BookView` çağırırken saniyede 1000 olay uygulanır.
    - Beklenen: yarış dedektörü temiz; okunan görüntü tutarlı (aynı çağrıda dönen seviyeler tek `LastUpdateID`'ye ait).
    - Kaynak: SPEC#13; ortak doğrulama `go test -race`.
88. **W1B-BOOK-26 — 5000 seviye snapshot performansı** `[unit]`
    - Girdi: `depth_limit=5000`, her tarafta 5000 seviye; ardından 10 000 diff olayı.
    - Beklenen: tahsis/olay sabit sınırda (benchmark); `big.Rat` iç temsilde kullanılmaz (yalnız dışa verirken).
    - Kaynak: SPEC#3(b) ("5000 seviyede big.Rat maliyetinden kaçınmak için").
89. **W1B-BOOK-27 — Defter kural tabanlı fuzz** `[fuzz]`
    - Girdi: rastgele `(U,u)` dizileri + rastgele snapshot `lastUpdateId`; referans model (dokümandaki kuralların düz uygulaması).
    - Beklenen: uygulama ile referans model her adımda aynı karar (apply / ignore / rebuild) ve aynı defter durumu; panik yok.
    - Kaynak: WSS:How to manage; SPEC#15 (fuzz).
90. **W1B-BOOK-28 — Canlı defter tutarlılığı** `[live]`
    - Girdi: 24 sa observe, BTCUSDT/ETHUSDT/SOLUSDT, `depth_limit=1000`.
    - Beklenen: her boşluk/çapraz olay yeniden kurmayla kurtarılmış; periyodik (ör. 15 dk) kontrol snapshot'ı ile yerel defterin ilk 20 seviyesi, snapshot `lastUpdateId`'si ile aynı noktaya getirildiğinde eşit (ölçüm olarak raporlanır, ağırlık bütçesinde).
    - Kaynak: SPEC#21 ("tüm defter tutarsızlıkları kurtarılmış").

## 8. Sabit nokta ölçeği (FIX)

91. **W1B-FIX-01 — Ölçek tick/step'ten** `[unit]`
    - Girdi: `tickSize="0.01000000"`, `stepSize="0.00001000"`.
    - Beklenen: fiyat ölçeği 2, miktar ölçeği 5 (sondaki sıfırlar ölçeğe katılmaz); `"4.00000000"` → `400`; `"431.00000000"` → `43100000`.
    - Kaynak: SPEC#3(b) ("ölçek sembolün tick/step ondalık basamağıdır"); FILT:PRICE_FILTER, LOT_SIZE.
92. **W1B-FIX-02 — Tam sayı tick** `[unit]`
    - Girdi: `tickSize="1.00000000"`, `"10.00000000"`; fiyat `"65000.00000000"`.
    - Beklenen: ölçek 0; `65000`. (`10` için ölçek negatif değil 0.)
    - Kaynak: SPEC#3(b); FILT.
93. **W1B-FIX-03 — Ölçeğe tam oturan değerler (sınır)** `[unit]`
    - Girdi: ölçek 2; `"0.01"`, `"0.10"`, `"0.00"`, `"0"`, `"92233720368547758.07"` (`int64` en büyük değer / 100).
    - Beklenen: `1, 10, 0, 0, 9223372036854775807`; taşma yok.
    - Kaynak: SPEC#3(b).
94. **W1B-FIX-04 — `tickSize=0` (devre dışı) → snapshot'tan ölçek** `[unit]`
    - Girdi: `PRICE_FILTER` `tickSize="0.00000000"` (ya da `TickSize` nil); snapshot fiyatları `"4.00000200"`, `"4.00000000"`, `"3.5"`.
    - Beklenen: fiyat ölçeği = en büyük ondalık basamak sayısı = 8; `"4.00000200"` → `400000200`.
    - Kaynak: SPEC#3(b); FILT ("tickSize … disabled on tickSize == 0").
95. **W1B-FIX-05 — `stepSize=0` / nil → snapshot'tan miktar ölçeği** `[unit]`
    - Girdi: `StepSize` nil; snapshot miktarları `"431.00000000"`, `"12.5"`.
    - Beklenen: miktar ölçeği 8 (FIX-06 yorumuna göre). Not: LOT_SIZE için "0 = devre dışı" FILT'te açıkça yazılı değildir (yalnız PRICE_FILTER için yazılı); spesifikasyon nil/0'ı aynı yola sokar.
    - Kaynak: SPEC#3(b); FILT:LOT_SIZE.
96. **W1B-FIX-06 — Sondaki sıfırlar ve en büyük basamak yorumu** `[unit]`
    - Girdi: `TickSize` nil; snapshot fiyatları yalnız `"4.00000000"` ve `"5.00000000"`.
    - Beklenen: belirsiz (gerilim 8). Savunmacı: yazılı basamak = 8; alternatif yorumda 0 seçilirse ilk `"4.01"` diff'i FIX-08 yolunu (ölçek büyütme + yeniden kurma) tetikler — orkestratör yorumu seçer ve bu test o yorumu sabitler.
    - Kaynak: SPEC#3(b); REST:Order book örneği (8 basamak sabit genişlik).
97. **W1B-FIX-07 — `tickSize=0` ve boş snapshot** `[unit]`
    - Girdi: `TickSize` nil; snapshot `bids: [], asks: []`.
    - Beklenen: belgelenmemiş; savunmacı: varsayılan ölçek (ör. 8, `baseAssetPrecision`/`quoteAssetPrecision` varsa o) ve ilk aşan değerde FIX-08 yolu; panik/sıfıra bölme yok.
    - Kaynak: SPEC#3(b); REST:Exchange information (`quoteAssetPrecision`).
98. **W1B-FIX-08 — Ölçeği aşan değer → ölçek büyütülür, defter yeniden kurulur** `[unit]`
    - Ön koşul: fiyat ölçeği 2, defter kurulu `localID=170`.
    - Girdi: `ev(171,171)` b: `[["4.005","1"]]`.
    - Beklenen: değer yuvarlanmaz/kesilmez; ölçek 3'e çıkar; defter yeniden kurulur (snapshot); `scale_changed{symbol, side:"price", from:2, to:3}` olayı; yeniden kurmadan sonra `4.005` doğru temsil edilir (`4005`).
    - Kaynak: SPEC#3(b) ("ölçeği aşan bir değer gelirse ölçek büyütülerek defter yeniden kurulur ve olay yazılır").
99. **W1B-FIX-09 — Miktar ölçeği aşımı** `[unit]`
    - Girdi: miktar ölçeği 5; `["4.00","0.000001"]`.
    - Beklenen: FIX-08 ile aynı, `side:"qty"`.
    - Kaynak: SPEC#3(b).
100. **W1B-FIX-10 — Aşan değer snapshot'ta** `[unit]`
     - Girdi: ölçek 2 (tickSize'dan); snapshot `"4.005"` içerir.
     - Beklenen: snapshot ölçeği 3'e büyütülerek yorumlanır (ya da yeniden istenir); olay yazılır; tick filtresine uymayan fiyat sessizce atılmaz.
     - Kaynak: SPEC#3(b).
101. **W1B-FIX-11 — `int64` taşması (fiyat × 10^ölçek)** `[unit]`
     - Girdi: ölçek 8; miktar `"92233720368.54775808"` (= 9223372036854775808 / 10^8, `MaxInt64+1`).
     - Beklenen: taşma tespit edilir (sessiz sarma yok); "aynı yol": yeniden kurma + `scale_overflow` olayı.
     - Kaynak: SPEC#3(b) ("int64 taşması da aynı yola gider").
102. **W1B-FIX-12 — Yeniden kurmadan sonra da taşma: döngü yok** `[unit]`
     - Girdi: FIX-11 değeri yeni snapshot'ta da var.
     - Beklenen: sonsuz yeniden kurma yok; sınırlı deneme sonra sembol `Stale` + tipli hata; diğer semboller etkilenmez. Ölçek büyütme taşmayı çözmez (gerilim 7); orkestratör "ölçek küçültme yok, sembol devre dışı" ya da başka bir kural seçmeli.
     - Kaynak: SPEC#3(b); savunmacı.
103. **W1B-FIX-13 — Ölçek büyütmenin kendisi taşma yaratır** `[unit]`
     - Ön koşul: ölçek 2, defterde `"90000000000000000.00"` (≈ 9e18, sığar).
     - Girdi: `"1.001"` gelir → ölçek 3 gerekiyor; mevcut seviye 10 ile çarpınca taşar.
     - Beklenen: taşma yolu (FIX-11/12); kısmi ölçek değiştirilmiş defter dışarı verilmez.
     - Kaynak: SPEC#3(b).
104. **W1B-FIX-14 — Geçersiz sayı dizgileri** `[unit]`
     - Girdi: `"-1.00"`, `"+1.00"`, `"1e-8"`, `"1E3"`, `".5"`, `"5."`, `" 5"`, `"5 "`, `""`, `"0x10"`, `"NaN"`, `"Inf"`, `"١٢٣"` (Arap-Hint rakamları), `"1.2.3"`, `"00012.5"`, 400 haneli dizgi.
     - Beklenen: negatif, işaretli, üstel, boşluklu, boş, onaltılık, NaN/Inf, ASCII dışı rakam, çoklu nokta → ret (bozuk mesaj). `".5"`/`"5."`/`"00012.5"`: belgelenmemiş biçimler; savunmacı ret ya da kabul — karar dokümante edilir ve tutarlı uygulanır. 400 hane: taşma yolu, CPU patlaması yok.
     - Kaynak: WSS/REST örnekleri yalnız `"d+.d+"` biçimi gösterir; savunmacı.
105. **W1B-FIX-15 — Dışa `*big.Rat` dönüşü birebir** `[unit]`
     - Girdi: ölçek 8, iç değer `400000200`.
     - Beklenen: `big.Rat` = `4000002/1000000` (= 4.000002); `FloatString(8)` = `"4.00000200"`; float64 ara adımı yok.
     - Kaynak: SPEC#3(b) ("dışarıya *big.Rat verilir").
106. **W1B-FIX-16 — Sabit nokta gidiş-dönüş fuzz** `[fuzz]`
     - Girdi: rastgele ondalık dizgi + rastgele ölçek 0..18.
     - Beklenen: `parse(s, scale)` ya tam değer ya "ölçek aşıldı" ya "taşma" hatası; başarılıysa `toRat(parse(s))` == `big.Rat.SetString(s)`; panik yok.
     - Kaynak: SPEC#15 ("Fuzz: … sabit nokta dönüşümü").

## 9. Kapanmış mumlar (KLINE)

107. **W1B-KLINE-01 — Yalnız `x=true` yayınlanır** `[unit]`
     - Girdi: `t=T0, T=T0+59999` için 29 güncelleme `x=false`, ardından 1 güncelleme `x=true`.
     - Beklenen: tek `ClosedKline{t:T0, T:T0+59999, o,h,l,c,v,…}`; açık güncellemeler yayınlanmaz.
     - Kaynak: SPEC#3(c), SPEC#15 ("açık mum yayınlanmaz"); WSS:Kline (`x` "Is this kline closed?").
108. **W1B-KLINE-02 — Ardışık mumlar ve sıra** `[unit]`
     - Girdi: `1m` için `x=true` olayları `t=T0`, `T0+60000`, `T0+120000`.
     - Beklenen: üç yayın, `t` kesin artan; her `T == t + 60000 − 1`.
     - Kaynak: WSS:Kline örneği (`T` = `t` + 59999).
109. **W1B-KLINE-03 — Akış içi boşluk (bir kapanmış mum hiç gelmedi)** `[unit]`
     - Ön koşul: son yayın `t=T0`.
     - Girdi: sonraki `x=true` `t=T0+180000`.
     - Beklenen: `T0+60000` ve `T0+120000` eksik → REST `GET /api/v3/klines?symbol=BTCUSDT&interval=1m&startTime=1795132860000&endTime=1795132979999&limit=…` (piyasa hostu, ağırlık 2); dönen iki mum önce, sonra `T0+180000` yayınlanır; yayın sırası kesin artan.
     - Kaynak: SPEC#3(c) ("boşluk → REST klines ile doldurma"); REST:Kline (startTime/endTime, weight 2).
110. **W1B-KLINE-04 — Açık mum `t` değişti ama önceki `x=true` gelmedi** `[unit]`
     - Ön koşul: son açık güncelleme `t=T0, x=false`.
     - Girdi: `t=T0+60000, x=false`.
     - Beklenen: `T0` mumu kapanmış ama görülmemiş → REST ile `T0` doldurulur ve yayınlanır.
     - Kaynak: SPEC#3(c); savunmacı (kapanış olayının kaçırılması da boşluktur).
111. **W1B-KLINE-05 — Yeniden bağlanma sonrası boşluk** `[unit]`
     - Ön koşul: son yayın `t=T0`; bağlantı 5 dk 30 sn kopuk.
     - Girdi: yeni bağlantının ilk `x=true` mumu `t=T0+360000`.
     - Beklenen: 5 eksik mum (`T0+60000..T0+300000`) REST'ten, sırayla; ardından `T0+360000`; kayıp 0.
     - Kaynak: SPEC#3(c), SPEC#21 ("kapanmış mum kaybı 0").
112. **W1B-KLINE-06 — REST sayfalama (> 1000 eksik)** `[unit]`
     - Ön koşul: aralık `1s`; 25 dk kopukluk (1500 mum).
     - Girdi: sahte REST `limit=1000` başına en fazla 1000 döner.
     - Beklenen: en az 2 istek; ikinci isteğin `startTime` = ilk yanıttaki son `t` + 1000; toplam 1500 mum, yinelenme yok; her istek ağırlık 2.
     - Kaynak: REST:Kline ("limit Default 500; Maximum 1000"); REST:General ("With both, behaves like startTime but does not exceed endTime").
113. **W1B-KLINE-07 — REST açık mumu döndürür** `[unit]`
     - Girdi: REST yanıtının son elemanı `closeTime >= sunucu saati` (henüz kapanmamış).
     - Beklenen: REST yanıtında `x` alanı yok (belgelenmemiş kapanma bilgisi); savunmacı: `closeTime < serverTime` (SyncTime ofsetiyle) olmayan mum yayınlanmaz; WS'te `x=true` gelince yayınlanır.
     - Kaynak: REST:Kline (yanıt alanları; kapanma bayrağı yok); REST:General ("Without startTime or endTime, returns the most recent items").
114. **W1B-KLINE-08 — REST boş ya da eksik döner** `[unit]`
     - Girdi: 5 eksik mum için REST yalnız 3 döner (ör. işlem durdurulmuş sembol; ya da boş liste).
     - Beklenen: belgelenmemiş (işlemsiz aralıkta mum üretilip üretilmediği dokümanda yok); savunmacı: dönenler yayınlanır, eksik kalanlar `kline_gap_unfilled{t…}` olayıyla raporlanır, observe özetinde "kayıp" sayılır; sonsuz yeniden deneme yok (sınırlı).
     - Kaynak: belgelenmemiş.
115. **W1B-KLINE-09 — REST ve WS aynı mumu verir** `[unit]`
     - Girdi: REST doldurması `T0+60000`'ı yayınladıktan sonra gecikmeli WS `x=true` `t=T0+60000` gelir.
     - Beklenen: ikinci kez yayınlanmaz. Değerler farklıysa (belgelenmemiş) ilk yayın geçerli kalır, `kline_mismatch` olayı yazılır.
     - Kaynak: REST:Kline ("uniquely identified by their open time").
116. **W1B-KLINE-10 — REST hata ve geri çekilme** `[unit]`
     - Girdi: klines isteği 429 `Retry-After: 30`.
     - Beklenen: piyasa hostu 30 sn geri çekilir; WS'ten gelen yeni kapanmış mumlar boşluk doldurulana kadar tutulur (sıra korunur) ya da sıra dışı yayın sözleşmede açıkça izinliyse işaretlenir — savunmacı: tut; tampon sınırlı.
     - Kaynak: REST:IP Limits; SPEC#3(f).
117. **W1B-KLINE-11 — UTC gün sınırı** `[unit]`
     - Girdi: `1d` için `t=1795046400000` (2026-11-19 00:00 UTC), `T=1795132799999`; sonraki `t=T0`.
     - Beklenen: süreklilik doğrulanır (beklenen sonraki `t` = önceki `T + 1`); yerel saat dilimi `TZ=Europe/Istanbul` ya da `TZ=America/New_York` olsa da sonuç aynı.
     - Kaynak: WSS:Kline ("in UTC+0 timezone"); REST:Kline (startTime/endTime UTC).
118. **W1B-KLINE-12 — `1M` değişken uzunluk (Şubat)** `[unit]`
     - Girdi: `1M` mumları `t=1801440000000` (2027-02-01), `T=1803859199999`; sonraki `t=1803859200000` (2027-03-01). Artık yıl: `t=1832976000000` (2028-02-01), `T=1835481599999`, sonraki `t=1835481600000`.
     - Beklenen: boşluk yok olarak değerlendirilir (sabit 30 gün varsayımı yok; beklenen sonraki `t` = `T + 1`).
     - Kaynak: WSS:Kline intervals (`1M` months); süreklilik kuralı savunmacı.
119. **W1B-KLINE-13 — `1w` ve `3d` hizalaması** `[unit]`
     - Girdi: `1w` `t=1795392000000` (2026-11-23 Pazartesi 00:00 UTC).
     - Beklenen: haftanın hangi gün başladığı ve `3d` dönem başlangıcı dokümanda yok (belgelenmemiş); kod hizalamayı varsaymaz, `T + 1` kuralıyla ilerler; ilk mum doğrulamasız kabul edilir.
     - Kaynak: belgelenmemiş.
120. **W1B-KLINE-14 — Aralık sınırında saat kayması** `[unit]`
     - Girdi: yerel saat sunucudan 2 sn geride; `x=true` olayı yerel saatle `T`'den önce gelir.
     - Beklenen: yayın kararı yalnız `x` alanına göre (yerel saate göre değil); boşluk tespiti `t`/`T` alanlarıyla (yerel saatle değil).
     - Kaynak: SPEC#3(c) ("yalnız x=true").
121. **W1B-KLINE-15 — Geriye giden `t`** `[unit]`
     - Girdi: son yayın `t=T0+120000`; gelen `x=true` `t=T0+60000` (önceden yayınlanmış).
     - Beklenen: yinelenen olarak atılır; yeniden yayın yok.
     - Kaynak: savunmacı (örtüşen bağlantılar, ROT-11).
122. **W1B-KLINE-16 — Başlangıçta geçmiş yok** `[unit]`
     - Girdi: süreç başlar; ilk `x=true` `t=T0+600000`.
     - Beklenen: başlangıç öncesi doldurma yapılmaz (spesifikasyon tanımlamıyor); ilk mum yayınlanır ve süreklilik referansı olur. (W3 geçmiş verisi ayrı.)
     - Kaynak: SPEC#3(c); belgelenmemiş başlangıç davranışı.
123. **W1B-KLINE-17 — Birden fazla aralık bağımsız** `[unit]`
     - Girdi: `1m` boşluğu varken `1h` akışı sürekli.
     - Beklenen: yalnız `1m` için REST; `1h` yayınları gecikmez.
     - Kaynak: SPEC#3(c).
124. **W1B-KLINE-18 — Canlı: 24 sa kayıp 0** `[live]`
     - Girdi: observe 24 sa, `1m`.
     - Beklenen: her sembol için 1440 ± 1 kapanmış mum, `t` ardışık, kayıp 0; REST doldurma sayısı raporlanır.
     - Kaynak: SPEC#21.

## 10. aggTrade yayını (TRADES)

125. **W1B-TRADES-01 — Alan eşlemesi** `[unit]`
     - Girdi: dokümandaki aggTrade örneği (`a=12345, p="0.001", q="100", f=100, l=105, T=1672515782136, m=true`).
     - Beklenen: `marketdata.Trade{ID:12345, Price:1/1000, Qty:100, FirstID:100, LastID:105, Time:…, BuyerIsMaker:true}`; `M` alanı yok sayılır.
     - Kaynak: WSS:Aggregate Trade; SPEC#13.
126. **W1B-TRADES-02 — Yinelenen ve sıra dışı `a`** `[unit]`
     - Girdi: `a` = 10, 11, 11, 9, 12.
     - Beklenen: yayın 10, 11, 12; `11` ikinci kez ve `9` atılır, sayaçlara yazılır.
     - Kaynak: savunmacı (örtüşme, ROT-12).
127. **W1B-TRADES-03 — `a` boşluğu** `[unit]`
     - Girdi: `a` = 10, 11, 15.
     - Beklenen: aggTrade kimliklerinin ardışık olduğu belgelenmemiş; savunmacı: `15` yayınlanır, `trade_gap{from:12,to:14}` sayacı (bilgi amaçlı); doldurma spesifikasyonda yok.
     - Kaynak: belgelenmemiş.
128. **W1B-TRADES-04 — Yavaş simülatör tüketicisi** `[unit]`
     - Girdi: tüketici okumaz, saniyede 2000 işlem.
     - Beklenen: WS okuma goroutine'i bloklanmaz (CONN-04 ile aynı ilke); taşma sayacı ve olay; diğer akışlar (defter, kline) etkilenmez.
     - Kaynak: savunmacı.

## 11. Tazelik (FRESH)

129. **W1B-FRESH-01 — Eşik sınırı** `[unit]`
     - Ön koşul: `max_data_age_ms=5000`; sahte monotonik saat.
     - Girdi: BTCUSDT son olay `t=0`; sorgu `t=4999`, `t=5000`, `t=5001`.
     - Beklenen: `4999` ve `5000` taze; `5001` stale ("aşımı" = kesin büyük). Sınır kapsayıcılığı dokümante edilir.
     - Kaynak: SPEC#3(e) ("risk.max_data_age_ms aşımı → stale").
130. **W1B-FRESH-02 — Olay zamanı yerel alış zamanıdır** `[unit]`
     - Girdi: bookTicker (E alanı yok) ve depthUpdate (`E` sunucu saatinden 3 sn geride).
     - Beklenen: tazelik yerel monotonik alış zamanıyla ölçülür (bookTicker'da `E` yok); ayrıca `E` varsa sunucu gecikmesi (`now − E`, SyncTime ofsetiyle) ayrı metrik olarak raporlanır.
     - Kaynak: WSS:Book Ticker (payload'da `E` yok); E§A ("It has no event type and no event time").
131. **W1B-FRESH-03 — Sembol başına bağımsız** `[unit]`
     - Girdi: BTCUSDT olay alıyor, ETHUSDT 6 sn olaysız.
     - Beklenen: yalnız ETHUSDT stale.
     - Kaynak: SPEC#3(e) ("sembol başına").
132. **W1B-FRESH-04 — Hangi olaylar tazeler** `[unit]`
     - Girdi: yalnız ping ve yalnız açık kline güncellemesi (`x=false`) gelen sembol.
     - Beklenen: ping tazelemez. Açık kline güncellemesi: spesifikasyon "son olay zamanı" diyor; savunmacı: herhangi bir piyasa verisi olayı (kline dahil) tazeler, ama `BookView.Stale` ayrıca defter durumuna bağlıdır (yeniden kurma sırasında true).
     - Kaynak: SPEC#3(e), SPEC#13.
133. **W1B-FRESH-05 — Stale süresi raporu** `[unit]`
     - Girdi: 10 sn kopukluk + yeniden kurma 2 sn.
     - Beklenen: sembol başına toplam stale süresi ≈ 12 sn − 5 sn eşik (tanım dokümante edilir) raporlanır; observe özetinde görünür.
     - Kaynak: SPEC#21 ("stale süresi raporlandı").
134. **W1B-FRESH-06 — Duvar saati sıçraması tazeliği bozmaz** `[unit]`
     - Girdi: duvar saati −1 sa geri alınır.
     - Beklenen: stale hesabı monotonik saatle; negatif yaş yok; yanlış "taze" yok.
     - Kaynak: savunmacı.

## 12. REST ağırlık bütçesi ve host bazlı geri çekilme (REST)

135. **W1B-REST-01 — Ağırlık sayacı başlıktan senkronlanır** `[unit]`
     - Girdi: yerel sayaç 100; yanıt `X-MBX-USED-WEIGHT-1M: 350`.
     - Beklenen: sayaç 350'ye çıkar (sunucu değeri yüksekse benimsenir). Başlık yerel sayaçtan düşükse: yeni dakika penceresi olabilir (belgelenmemiş pencere hizası); savunmacı: düşük değer yalnız dakika sınırı geçtiyse benimsenir.
     - Kaynak: SPEC#3(f); REST:IP Limits ("Every request will contain X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)").
136. **W1B-REST-02 — Başlık yok / bozuk** `[unit]`
     - Girdi: başlık yok; `abc`; `-5`; `99999999999999999999`.
     - Beklenen: yerel tahmin korunur; panik yok; uyarı sayacı.
     - Kaynak: savunmacı (REST "every request will contain" diyor; yokluğu belgelenmemiş).
137. **W1B-REST-03 — Limit `exchangeInfo.rateLimits`'ten** `[unit]`
     - Girdi: `rateLimits` içinde `{"rateLimitType":"REQUEST_WEIGHT","interval":"MINUTE","intervalNum":1,"limit":6000}`; ikinci test `limit:1200`.
     - Beklenen: bütçe sırasıyla 6000 ve 1200; sabit kodlanmış değer kullanılmaz; `REQUEST_WEIGHT` yoksa ya da `interval` MINUTE/1 değilse savunmacı muhafazakâr varsayılan + uyarı.
     - Kaynak: REST:General Info on Limits; REST:Exchange information (rateLimits örneği); ENUM:Rate limiters.
138. **W1B-REST-04 — %20 emir/iptal rezervi** `[unit]`
     - Ön koşul: limit 6000 → piyasa verisi / sorgu trafiği tavanı 4800; emir/iptal sınıfı 6000'e kadar.
     - Girdi: kullanılan 4760; depth snapshot (50) rezerv isteği; ardından iptal sınıfı 1 ağırlık isteği.
     - Beklenen: snapshot 4810 > 4800 olacağından bekletilir (dakika penceresi dönene kadar ya da başlık düşene kadar); iptal hemen geçer.
     - Kaynak: SPEC#3(f) ("ağırlığın %20'si emir/iptal yoluna ayrılır (E-13)"); E-13.
139. **W1B-REST-05 — Rezervasyon eşzamanlılığı** `[unit]`
     - Girdi: kullanılan 4700; 3 goroutine aynı anda 50'lik snapshot rezervi ister.
     - Beklenen: en fazla 2'si geçer (4800 tavanı); üçüncüsü bekler; yarış dedektörü temiz; rezerv istek atılmadan önce yapılır (yanıt sonrası değil).
     - Kaynak: SPEC#3(f).
140. **W1B-REST-06 — Rezervasyon iadesi** `[unit]`
     - Girdi: rezerv alınmış istek ağa çıkmadan context iptal ediliyor; ikinci durumda istek ağa çıktı ve 500 döndü.
     - Beklenen: ağa çıkmayan istek rezervi iade edilir; ağa çıkan istek ağırlık harcamış sayılır (sunucu başlığıyla düzeltilir).
     - Kaynak: savunmacı.
141. **W1B-REST-07 — Dakika penceresi dönüşü** `[unit]`
     - Girdi: `12:00:59.900`'de kullanılan 4800; `12:01:00.100`'de istek.
     - Beklenen: pencere hizası belgelenmemiş (takvim dakikası mı kayan mı); savunmacı: yerel pencere muhafazakâr (kayan) ve sunucu başlığı belirleyici; pencere dönüşünde bekleyen istekler sırayla geçer.
     - Kaynak: REST:IP Limits; belgelenmemiş hizalama.
142. **W1B-REST-08 — 429 piyasa hostunda imzalı hostu engellemez** `[unit]`
     - Girdi: `data-api.binance.vision` (sahte) 429 `Retry-After: 60` döner; hemen ardından imzalı host (sahte `api.binance.com`) için istek.
     - Beklenen: piyasa hostu 60 sn geri çekilir; imzalı host isteği beklemeden gider.
     - Kaynak: SPEC#15 ("piyasa hostundan 429 imzalı hostu engellemez"); E-13.
143. **W1B-REST-09 — İmzalı host 429 piyasa hostunu engellemez** `[unit]`
     - Girdi: tersi.
     - Beklenen: piyasa hostu etkilenmez.
     - Kaynak: SPEC#3(f) ("host bazlı ayrı geri çekilme").
144. **W1B-REST-10 — `Retry-After` uygulanır ve sınırlanır** `[unit]`
     - Girdi: 429 `Retry-After: 7`; 418 `Retry-After: 259200` (3 gün); 418 `Retry-After: 999999`; 429 başlıksız; `Retry-After: -1`; `Retry-After: Wed, 21 Oct 2026 07:28:00 GMT` (HTTP-date biçimi).
     - Beklenen: 7 sn; 72 sa; 72 sa'e kırpılır (mevcut 1 sn–72 sa sınırı); başlıksız → üstel varsayılan; negatif → 1 sn; HTTP-date: Binance saniye cinsinden belgeliyor, tarih biçimi belgelenmemiş — savunmacı: ayrıştırılamazsa varsayılan.
     - Kaynak: REST:IP Limits ("Retry-After header … number of seconds"; ban "from 2 minutes to 3 days"); 01-waves §1 (1 sn–72 sa).
145. **W1B-REST-11 — 403 WAF ve `-1003`** `[unit]`
     - Girdi: 403; 429 gövdesi `{"code":-1003,"msg":"Too much request weight used; …"}`.
     - Beklenen: 403 → host için geri çekilme ve tipli `ErrWAF` (rate limit ya da güvenlik bloğu olabilir); `-1003` → rate limit tipli hata.
     - Kaynak: REST:HTTP Return Codes (403 WAF); ERR:-1003.
146. **W1B-REST-12 — Mevcut istemci testleri korunur** `[unit]`
     - Girdi: mevcut `internal/exchange/binance` test paketi.
     - Beklenen: değişiklik sonrası hepsi değişmeden geçer.
     - Kaynak: SPEC#10 ("mevcut testler korunur").
147. **W1B-REST-13 — Ağırlık havuzu paylaşımı ölçümü** `[live]`
     - Girdi: `data-api.binance.vision`'a 5 depth (limit 1000) isteği; ardından `api.binance.com/api/v3/time`.
     - Beklenen: iki hostun `X-MBX-USED-WEIGHT-1M` başlıkları kaydedilir; ikinci host 250+ gösteriyorsa havuz paylaşımlıdır → rezerv tasarımı tek IP sayacına çevrilir (gerilim 5). Sonuç `docs/exchanges/binance.md`'ye yazılır.
     - Kaynak: REST:IP Limits ("based on the IPs"); belgelenmemiş host ilişkisi.

## 13. Yaşam döngüsü, context iptali, goroutine'ler (LIFE)

148. **W1B-LIFE-01 — Context iptalinde zaman sınırında kapanış** `[unit]`
     - Ön koşul: 3 sembol, 2 bağlantı (biri devir ortasında), bir snapshot uçuşta, bir REST kline doldurması geri çekilmede.
     - Girdi: context iptal.
     - Beklenen: `WaitGroup` 2 sn (dokümante sınır) içinde sıfırlanır; `runtime.NumGoroutine()` başlangıç değerine döner (sızıntı yok); WS bağlantıları close frame ile kapanır; dönüş hatası `context.Canceled`.
     - Kaynak: SPEC#15 ("context iptalinde tüm goroutine'ler WaitGroup ile zaman sınırında biter").
149. **W1B-LIFE-02 — Dial sırasında iptal** `[unit]`
     - Girdi: sunucu upgrade'e hiç cevap vermiyor; 100 ms sonra iptal.
     - Beklenen: dial hemen döner; goroutine sızıntısı yok.
     - Kaynak: SPEC#15.
150. **W1B-LIFE-03 — Snapshot isteği sırasında iptal** `[unit]`
     - Girdi: sahte REST yanıtı süresiz bekletir; iptal.
     - Beklenen: HTTP isteği iptal; ağırlık rezervi muhasebesi tutarlı (REST-06); yeniden kurma kuyruğu boşaltılır.
     - Kaynak: SPEC#15.
151. **W1B-LIFE-04 — Tüketici kanalı okunmuyorken iptal** `[unit]`
     - Girdi: tüm çıkış kanalları dolu ve okunmuyor; iptal.
     - Beklenen: gönderen goroutine'ler `select` ile iptali görür, bloklanmaz; kapanış sınırı içinde.
     - Kaynak: SPEC#15.
152. **W1B-LIFE-05 — Çift kapatma / iptal sonrası çağrı** `[unit]`
     - Girdi: `Close()` iki kez; iptal sonrası `BookView` çağrısı.
     - Beklenen: panik yok; ikinci `Close` no-op; `BookView` `Stale=true` ya da tipli "kapalı" hatası.
     - Kaynak: savunmacı.
153. **W1B-LIFE-06 — Panik izolasyonu** `[unit]`
     - Girdi: test kancasıyla defter uygulayıcısında panik (ör. fuzz bulgusu simülasyonu).
     - Beklenen: savunmacı: süreç çökmez ya da kontrollü çöker — spesifikasyon tanımlamıyor; tercih: sembol `Stale`, olay, diğer semboller çalışır. Karar dokümante edilir.
     - Kaynak: belgelenmemiş (savunmacı).

## 14. `observe` komutu ve canlı doğrulama (OBS)

154. **W1B-OBS-01 — JSONL çıktısı ve süre** `[unit]`
     - Ön koşul: sahte WS + sahte REST; `-duration 3m` (sahte saat).
     - Girdi: `traderd observe -config f -symbols BTCUSDT,ETHUSDT -duration 3m -out o.jsonl`.
     - Beklenen: her satır geçerli JSON; olay tipleri (kline, reconnect, rotation, book_rebuild, stale, scale_changed…) belgelenmiş alanlarla; son satır özet: sembol başına kapanmış mum sayısı, kayıp, defter tutarsızlığı/kurtarma sayısı, stale süresi, yeniden bağlanma sayısı. Süre bitiminde çıkış kodu 0.
     - Kaynak: SPEC#12, SPEC#18, SPEC#21.
155. **W1B-OBS-02 — SIGINT/SIGTERM'de özet** `[unit]`
     - Girdi: 30 sn sonra SIGINT.
     - Beklenen: context iptali (LIFE-01), özet satırı yazılır, dosya fsync'lenir, çıkış kodu dokümante (ör. 0 ya da 130).
     - Kaynak: SPEC#18 ("son özet satırı paylaşılır"); savunmacı.
156. **W1B-OBS-03 — Çıktıda sır ve ham büyük veri yok** `[unit]`
     - Girdi: normal koşu.
     - Beklenen: API anahtarı kullanılmaz (salt piyasa verisi); defterin tamamı her olayda yazılmaz (dosya boyutu 24 sa'te makul; sınır dokümante).
     - Kaynak: SPEC#4 (imzalı akış yok); 00-overview (loglarda sır yok).
157. **W1B-OBS-04 — Canlı 24 sa koşu** `[live]`
     - Girdi: `go run ./cmd/traderd observe -config configs/example.yaml -symbols BTCUSDT,ETHUSDT,SOLUSDT -duration 24h -out observe.jsonl`.
     - Beklenen: kapanmış mum kaybı 0; tüm defter tutarsızlıkları kurtarılmış; stale süresi raporlandı; gözlenen en büyük WS mesaj boyutu ve `serverShutdown` sayısı raporlanır (PARSE-09 sınırı için girdi).
     - Kaynak: SPEC#18, SPEC#21, SPEC#6.
158. **W1B-OBS-05 — Canlı: depth@100ms sessiz piyasa davranışı** `[live]`
     - Girdi: düşük hacimli bir USDT paritesi 1 sa.
     - Beklenen: değişiklik yokken depth olayı gelip gelmediği belgelenmemiş; ölçülen en uzun olaysız aralık raporlanır; `max_data_age_ms=5000` ile yanlış stale oranı hesaplanır (R-9 eşiği için girdi).
     - Kaynak: WSS:Diff Depth ("Update Speed: 1000ms or 100ms"); boş güncelleme davranışı belgelenmemiş.
159. **W1B-OBS-06 — Canlı: sembol kontrolü** `[live]`
     - Girdi: `-symbols BTCUSDT,NOTAREALCOIN`.
     - Beklenen: savunmacı: exchangeInfo'da olmayan sembol bağlantı açılmadan ret. Var olmayan akışa WS'in nasıl davrandığı belgelenmemiş; bu koşuda gözlenir ve kaydedilir.
     - Kaynak: REST:Exchange information ("If the value provided to symbol or symbols do not exist, the endpoint will throw an error").

---

## Sayım

Senaryolar 1–159 arası sıralı numaralıdır; her ID benzersizdir.

| Bileşen | Senaryo | unit | fuzz | live |
| --- | --- | --- | --- | --- |
| CFG | 10 | 10 | 0 | 0 |
| CONN | 12 | 11 | 0 | 1 |
| ROT | 14 | 13 | 0 | 1 |
| RECON | 9 | 9 | 0 | 0 |
| LIM | 3 | 3 | 0 | 0 |
| PARSE | 14 | 13 | 1 | 0 |
| BOOK | 28 | 26 | 1 | 1 |
| FIX | 16 | 15 | 1 | 0 |
| KLINE | 18 | 17 | 0 | 1 |
| TRADES | 4 | 4 | 0 | 0 |
| FRESH | 6 | 6 | 0 | 0 |
| REST | 13 | 12 | 0 | 1 |
| LIFE | 6 | 6 | 0 | 0 |
| OBS | 6 | 3 | 0 | 3 |
| **Toplam** | **159** | **148** | **3** | **8** |
