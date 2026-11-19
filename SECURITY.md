# Güvenlik

## Sırlar

- Borsa ve Anthropic API anahtarları repoya, config dosyalarına, test verisine, loglara veya hata mesajlarına girmez.
- Sunucuda anahtarlar `systemd-creds` (`LoadCredentialEncrypted`) ya da SOPS + age ile şifreli dosyada tutulur. Ortam değişkeni kullanılmaz (wave W7).
- Borsa anahtarlarında **para çekme yetkisi kapalıdır**. Destekleniyorsa anahtar VPS IP'sine kısıtlanır.
- Paper, küçük canlı ve tam aşama için ayrı anahtarlar kullanılır. Kill switch'in ayrı bir anahtarı vardır.
- Bir anahtarın sızdığından şüphelenilirse anahtar borsada hemen silinir ve yenisi oluşturulur.

## LLM güvenliği

- LLM ajanlarının aracı (tool), ağ erişimi, anahtarı ya da emir yetkisi yoktur.
- Haber ve duyuru metni güvenilmez girdidir. Bu metni yalnızca karantinadaki olay ajanı okur; diğer ajanlar ham metni görmez.

## Bildirim

Bu kişisel bir projedir. Bir güvenlik sorunu fark edersen repoda public issue açma; repo sahibine doğrudan ulaş.
