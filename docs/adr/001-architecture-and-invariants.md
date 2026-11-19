# ADR-001: Mimari ve değişmezler

- Durum: Kabul edildi (2026-11-19)
- Kaynak: Plan dokümanı, kararlar D-1..D-16

## Bağlam

Sistem kendi sermayesiyle (planlanan 200 USD), Binance TR TRY paritelerinde spot işlem yapacak. Hedef tam otonom canlı işlem; geçiş paper → küçük canlı → tam şeklinde, her kapıda kullanıcı onayıyla. Araştırma, LLM'lerin fiyat tahmininde güvenilir olmadığını, aritmetik ve şema hatası yaptığını ve haber metni üzerinden prompt injection'a açık olduğunu gösteriyor.

## Karar

Dört katman:

1. **Veri katmanı (Go, deterministik):** borsa adaptörleri, piyasa verisi, özellik motoru, haber alımı.
2. **LLM ajanları (Claude API):** yalnızca JSON şemasına uyan öneri üretir. Aracı, ağ erişimi, anahtarı ve emir yetkisi yoktur.
3. **Deterministik çekirdek (Go):** sinyal motoru, boyutlandırma, risk motoru, yürütme, mutabakat, kill switch. Son söz buradadır.
4. **Öğrenme düzlemi (Python, çevrimdışı):** backtest, değerlendirme, ders karantinası, politika kaydı.

Değişmezler (kod ve testlerle zorlanır):

- **I-1** LLM çıktısı emir değildir; önce şema doğrulamasından, sonra risk motorundan geçer.
- **I-2** LLM sayı hesaplamaz; fiyat, gösterge, boyut ve maliyet koddan gelir.
- **I-3** Intraday giriş ve çıkış kural tabanlıdır.
- **I-4** LLM erişilemezse ya da maliyet tavanı aşılırsa sistem güvenli moda geçer; açık pozisyonlar borsada duran stoplarla korunur.
- **I-5** Öğrenme çevrimdışıdır; canlıya yalnızca sürümlü, testli ve onaylı politika paketi çıkar.
- **I-6** Her karar değişmez bir günlüğe yazılır.
- **I-7** Borsa API anahtarlarında para çekme yetkisi yoktur.

## Sonuçlar

- LLM'in katkısı ayrı ayrı ölçülebilir, çünkü her rol konfigürasyonla kapatılabilir ve kapalıyken deterministik bir varsayılan devreye girer.
- Tek Go modülü (`github.com/cembasaranoglu/trader`); çalıştırılabilir dosyalar `cmd/` altında, uygulama `internal/` altında. `pkg/` yok, çünkü dışarıya açılan kararlı bir API yok.
- **Assumption:** Go 1.24.7, geliştirme ortamında doğrulanabilen sürüm olduğu için seçildi. Daha yeni bir sürüme geçmek `go.mod` güncellemesi ve CI'ın yeşil kalmasıyla yapılır.
