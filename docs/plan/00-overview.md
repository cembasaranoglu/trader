# Uygulama planı v2

Bu klasör kalan tüm işlerin kodlanabilir spesifikasyonudur. Kararlar ve gerekçeler [plan dokümanında](https://claude.ai/artifact/7Rgd1jjuXpecY2bMzy1NAH) (D-1..D-17) ve `docs/adr/` altındadır. Bu dosyalar o kararları uygulanabilir işlere çevirir. Planın gerektirdiği yeni kararlar ve önerilerim `01-waves.md` §4'te (U-1..U-22) **decision needed** olarak listelidir; onaylanmadan ilgili dalga başlamaz.

Etiketler: **fact** (kaynaklı ya da ölçülmüş), **decision** (kullanıcı kararı), **recommendation** (onay bekler), **unknown**, **blocker**.

## Üretime hazır tanımı (her dalga için geçerli)

Bir dalga ancak aşağıdakilerin hepsi sağlanınca "tamam" sayılır:

1. **Doğrulama:** `gofmt -l .` boş; `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` temiz; `go test -race -count=1 ./...` geçer; CI yeşil.
2. **Test kapsamı:** risk, yürütme, mutabakat ve kill switch paketlerinde satır kapsamı ≥ %90; diğer paketlerde ≥ %80. Kapsam sayısı tek başına yeterli değildir: her risk kuralı için sınır, sıfır, negatif ve ihlal vakası tablo testi olarak bulunur.
3. **Dış çağrılar:** her ağ çağrısı `context.Context` alır, zaman aşımı vardır, yeniden deneme sayısı sınırlıdır, hata tipli döner (`errors.Is/As`).
4. **Çökme güvenliği:** süreç herhangi bir anda öldürülürse, yeniden başladığında borsadaki açık emirler ve pozisyonlar mutabakatla geri okunur; korumasız pozisyon kalmaz (stoplar borsada durur, R-24).
5. **Gözlemlenebilirlik:** yapılandırılmış JSON log (`log/slog`), yalnız localhost'a açık metrik ucu, kritik olaylarda alarm. Loglarda sır yok.
6. **Güvenlik:** sır yok (repo, log, hata, rapor); başlangıçta API anahtarının çekim yetkisi borsadan okunur ve açıksa süreç başlamaz.
7. **Dokümantasyon:** README, ilgili runbook ve ADR güncel; davranış değişikliği dokümanla çelişmez.
8. **Canlı doğrulama:** borsa davranışına dayanan her özellik, testnet veya Demo ortamında kullanıcının makinesinde/VPS'te çalıştırılan bir doğrulama komutuyla kanıtlanır. Geliştirme ortamı borsalara erişemediği için bu adım atlanamaz.

## Uygulama rolleri

| Rol | Kim | Sorumluluk | Sınır |
| --- | --- | --- | --- |
| Orkestratör | Claude (ana oturum) | Dalga spesifikasyonu, kodlama, entegrasyon, commit ve push, plan dokümanının güncel tutulması | Kullanıcı kararı gereken noktada durur ve sorar |
| Test tasarımcısı | Bağımsız ajan, her dalgada | Spesifikasyondan, koda bakmadan önce kenar durum ve hata senaryosu listesi çıkarır | Kod yazmaz; senaryo listesi üretir |
| Kod inceleyici | Bağımsız ajan, her dalga sonunda | Doğruluk, güvenlik, sağlamlık incelemesi; bulgular düzeltilmeden dalga kapanmaz | Dosya değiştirmez |
| Araştırmacı | Bağımsız ajan, ihtiyaç halinde | Borsa ve API davranışını resmi kaynaktan doğrular | Tasarım kararı vermez |
| Canlı doğrulayıcı | Kullanıcı | Doğrulama komutlarını kendi makinesinde/VPS'te çalıştırıp çıktıyı paylaşır | Sır paylaşmaz |
| Karar verici | Kullanıcı | Kapı onayları, risk değerleri, kapsam kararları | — |

## Dalga sırası

Bkz. [01-waves.md](01-waves.md).
