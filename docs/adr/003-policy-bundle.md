# ADR-003: Politika paketi formatı

- Durum: Kabul edildi (2026-11-19); uygulama W4'te başlar
- Kaynak: Plan dokümanı, "Öğrenme döngüsü" bölümü

## Bağlam

LLM davranışını prompt metni, çıktı şeması, model ID'si ve effort ayarı birlikte belirler. Claude çıktıları deterministik değildir. Bu yüzden tekrarlanabilirlik, kullanılan her şeyin sürümlenmesine ve kaydedilmesine dayanır. Öğrenme döngüsünün ürettiği her değişiklik de geri alınabilir olmalı.

## Karar

Bir **politika paketi**, `policies/vNNNN/` altında değişmez bir dizindir:

```text
policies/v0001/
  manifest.yaml       # sürüm, oluşturma tarihi, üst sürüm, değişiklik nedeni, onay kaydı
  models.yaml         # ajan başına model ID ve effort
  prompts/<ajan>.md   # sistem promptları
  schemas/<ajan>.json # yapılandırılmış çıktı şemaları
  params.yaml         # sinyal ve boyutlandırma parametreleri
  memory/             # onaylı ders ve few-shot snapshot'ı
  SHA256SUMS          # paket içeriğinin hash listesi
```

- Canlı politika bir işaretçidir. Terfi işaretçiyi yeni sürüme, geri alma eski sürüme çevirmektir.
- Yayınlanmış bir paket değiştirilmez. Her değişiklik yeni bir sürüm üretir.
- Süreç başlarken `SHA256SUMS` doğrulanır; uyuşmazlık varsa başlamaz.
- Her karar kaydı politika sürümünü içerir.

## Sonuçlar

- Deneme kayıt defteri (DSR/PBO için) her aday paketi sayar.
- Şema içindeki sembol enum'u yalnızca yeni bir paket sürümüyle değişir, çünkü şema değişikliği prompt cache'ini geçersiz kılar.
