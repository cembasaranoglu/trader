# Runbook: yerel Docker, VPS kurulumu ve GitHub'dan deploy

Kararlar (2026-11-19): yerel çalışma Docker ile; VPS ve yönetilen PostgreSQL AWS Tokyo Lightsail'de; deploy sürüm etiketiyle ve GitHub onayıyla; emirleri tek yürütücü verir, işçiler ölçeklenir (ADR-007).

## 1. Yerel geliştirme (Docker)

Gereken: Docker Engine ya da Docker Desktop (Compose v2).

```sh
docker compose up -d --wait postgres     # PostgreSQL 16 (yalnız 127.0.0.1:5432)
docker compose run --rm migrate          # sahip rolle migration + yetkiler
docker compose run --rm verify           # uygulama rolüyle doğrulama
docker compose --profile observe up -d observe   # piyasa verisi gözlemi (Binance erişimi gerekir)
docker compose logs -f observe
docker compose down                      # durdur (veri volume'da kalır); -v ile siler
```

`compose.yaml`'daki parolalar yalnız bu yerel konteyner içindir. Yerel config: `deploy/local/config.yaml` (örnek config ile aynı, yalnız konteyner yolları farklı; CI bunu test eder). Go testleri gerçek PostgreSQL ister:

```sh
export TRADER_TEST_PG='postgres://trader_owner:local-dev-only@127.0.0.1:5432/trader?sslmode=disable'
go test -race ./...
```

(`trader_owner` yerel konteynerde süper kullanıcıdır; testler her test için ayrı veritabanı ve roller oluşturur.)

## 2. VPS ve veritabanı kurulumu (bir kez)

### 2.1 Lightsail örneği (Tokyo)

1. Lightsail konsolu → Create instance → bölge **Tokyo (ap-northeast-1)** → Linux, **Ubuntu 24.04 LTS**. Plan: en küçük planla başlayın; `observe` raporundaki `runtime.heap_alloc_bytes` ve `sys_bytes` değerlerine göre gerekirse büyütün (ölçülmeden boyut seçmiyoruz).
2. Statik IP ekleyin. Networking → IPv4 firewall: yalnız SSH (22); mümkünse kaynak IP'yi kendi IP'nizle sınırlayın. Gelen başka port yok.
3. **Binance erişimini ölçün** (bölge kararı buna bağlı). VPS'te:
   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' https://api.binance.com/api/v3/ping
   curl -s -o /dev/null -w '%{http_code}\n' 'https://api.binance.com/api/v3/exchangeInfo?symbol=BTCUSDT'
   ```
   İkisi de `200` olmalı. `451` gelirse bu bölge kullanılamaz; durun ve haber verin.

### 2.2 Docker ve kullanıcılar

1. Docker Engine'i resmi talimatla kurun: <https://docs.docker.com/engine/install/ubuntu/> (Compose eklentisi dahil).
2. Otomatik güvenlik güncellemeleri: `sudo apt install unattended-upgrades` (Ubuntu'da varsayılan açık olabilir; `systemctl status unattended-upgrades` ile kontrol edin).
3. Deploy kullanıcısı ve klasörler:
   ```sh
   sudo adduser --disabled-password --gecos "" deploy
   sudo usermod -aG docker deploy          # docker grubu root'a eşdeğerdir; bu anahtar yalnız deploy.sh çalıştırabilir (2.4)
   sudo install -d -o deploy -g deploy -m 750 /opt/trader
   sudo install -d -o root -g root -m 755 /opt/trader/bin
   sudo install -d -o root -g root -m 755 /opt/trader/secrets
   sudo install -d -o 65532 -g 65532 -m 700 /opt/trader/secrets/app /opt/trader/secrets/owner
   sudo install -d -o 65532 -g 65532 -m 750 /var/lib/trader
   ```
   `65532`, imajdaki süreç kullanıcısıdır (distroless nonroot). Sırlar ona aittir; `deploy` kullanıcısı sırları okuyamaz. `secrets/app` her servise (`/run/secrets`), `secrets/owner` yalnız `migrate` servisine (`/run/secrets-owner`) bağlanır; çalışan servisler sahip rolün bağlantı dizesini görmez.
4. `deploy/prod/deploy.sh` dosyasını repodan VPS'e kopyalayın: `/opt/trader/bin/deploy.sh`, sahibi `root`, mod `755`. (Bu betik nadiren değişir; değişirse aynı yolla güncellenir.)

### 2.3 Lightsail yönetilen PostgreSQL

1. Lightsail → Databases → Create database → aynı bölge (Tokyo) → **PostgreSQL 16** → Standard plan. Ana kullanıcı adı ve parolayı kaydedin (parola yöneticisine; sohbete, repoya, loglara değil).
2. Public mode kapalı kalsın (varsayılan). Lightsail örneği aynı bölgede olduğu için özel ağdan bağlanır.
3. CA paketini indirin ve sırların yanına koyun (Lightsail: RDS etiketli paketler Lightsail için de geçerlidir):
   ```sh
   curl -fsSL https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem -o /tmp/rds-ca.pem
   sudo install -o 65532 -g 65532 -m 600 /tmp/rds-ca.pem /opt/trader/secrets/app/rds-ca.pem
   ```
   (Adres AWS'nin "Download an SSL/TLS certificate" sayfasındaki tüm bölgeler paketidir; indirmeden önce o sayfayla karşılaştırın.)
4. Roller ve veritabanı (VPS'ten, geçici bir psql konteyneriyle; ana parolayı istemde yazın, hiçbir dosyaya kaydetmeyin). Ana kullanıcı yalnız bu kurulumda kullanılır; VPS'te ana parola **saklanmaz**. Sahip rol (`trader_owner`) yalnız bu veritabanının sahibidir, rol oluşturamaz:
   ```sh
   docker run --rm -it -v /opt/trader/secrets/app/rds-ca.pem:/ca.pem:ro postgres:16-alpine@sha256:1d5b02d3a9941fbd52437db4107f8c52d6297dac553ca1e5c299ebb3756b4857 \
     psql "host=<UÇ NOKTA> port=5432 user=<ANA KULLANICI> dbname=postgres sslmode=verify-full sslrootcert=/ca.pem"
   ```
   ```sql
   CREATE ROLE trader_owner LOGIN PASSWORD '<uzun rastgele parola 1>';
   CREATE ROLE trader_app   LOGIN PASSWORD '<uzun rastgele parola 2>';
   GRANT trader_owner TO <ANA KULLANICI>;   -- PostgreSQL 16: başka bir role ait veritabanı oluşturmak için gerekli
   CREATE DATABASE trader OWNER trader_owner;
   REVOKE trader_owner FROM <ANA KULLANICI>;
   ```
   (Varsayım, ADR-007: Lightsail ana kullanıcısı rol oluşturabilir. Hata verirse durun ve haber verin.)
5. Bağlantı dizesi dosyaları (parolalar URL kodlu olmalı; örn. `@` → `%40`):
   ```sh
   sudo -u '#65532' sh -c 'umask 077; cat > /opt/trader/secrets/owner/db-owner.dsn' <<'X'
   postgres://trader_owner:<PAROLA 1>@<UÇ NOKTA>:5432/trader?sslmode=verify-full&sslrootcert=/run/secrets/rds-ca.pem
   X
   sudo -u '#65532' sh -c 'umask 077; cat > /opt/trader/secrets/app/db-app.dsn' <<'X'
   postgres://trader_app:<PAROLA 2>@<UÇ NOKTA>:5432/trader?sslmode=verify-full&sslrootcert=/run/secrets/rds-ca.pem
   X
   ```
   Kabuk geçmişine parola düşmemesi için heredoc kullanın; `sudo ls -l /opt/trader/secrets/app /opt/trader/secrets/owner` ile sahip `65532` ve mod `-rw-------` olduğunu kontrol edin. `sslrootcert` zorunludur: `traderd` yerel olmayan bir veritabanına açık bir CA dosyası olmadan bağlanmaz.

### 2.4 Deploy anahtarı (GitHub Actions → VPS)

Kendi bilgisayarınızda:

```sh
ssh-keygen -t ed25519 -f trader-deploy -C github-deploy -N ""
```

VPS'te `~deploy/.ssh/authorized_keys` (sahibi `deploy`, mod `600`, klasör `700`) içine tek satır:

```
restrict,command="/opt/trader/bin/deploy.sh" ssh-ed25519 AAAA...(trader-deploy.pub içeriği) github-deploy
```

`restrict` port yönlendirme, PTY ve diğer her şeyi kapatır; anahtar yalnız `deploy.sh`'yi çalıştırabilir, o da yalnız `ghcr.io/cembasaranoglu/trader@sha256:<digest>` biçimindeki imajı kabul eder.

VPS host anahtarını sabitleyin (ilk bağlantıda güvenme yok):

```sh
# VPS'te (Lightsail tarayıcı SSH'ı ile):
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
# Kendi bilgisayarınızda:
ssh-keyscan -t ed25519 <STATİK IP> > known_hosts.line
ssh-keygen -lf known_hosts.line        # parmak izi VPS'tekiyle aynı olmalı
```

### 2.5 GitHub ayarları

Repo **public** olduğu için environment onayı her planda kullanılabilir.

1. Settings → Environments → **New environment** `production`:
   - Required reviewers: `cembasaranoglu`.
   - Deployment branches and tags: Selected → tag kuralı `v*`.
   - Environment secrets: `VPS_HOST` (statik IP), `VPS_SSH_KEY` (`trader-deploy` özel anahtarının tamamı), `VPS_KNOWN_HOSTS` (`known_hosts.line` içeriği).
2. Settings → Actions → General → Workflow permissions: "Read repository contents" (varsayılan) kalsın; yayın iş akışı paket yazma iznini kendi ister.
3. İlk yayından sonra paket (`ghcr.io/cembasaranoglu/trader`) özel olarak oluşur ve repoya bağlıdır (imajdaki `org.opencontainers.image.source` etiketi). Görünürlüğü değiştirmeyin.
4. `trader-deploy` özel anahtarını secret'a koyduktan sonra bilgisayarınızdan silin.

## 3. Yayın ve deploy

```sh
git tag v0.1.0
git push origin v0.1.0
```

`release` iş akışı:

1. PostgreSQL 16 ile vet ve race testleri.
2. İmajı derler, `ghcr.io/cembasaranoglu/trader:v0.1.0` olarak gönderir, digest'i çıkarır.
3. `production` environment'ında **onayınızı** bekler (Actions → çalıştırma → Review deployments).
4. Onaydan sonra VPS'e bağlanır; `deploy.sh`:
   - kısa ömürlü token ile imajı digest'iyle çeker, sonra kimlik bilgisini siler;
   - imajın içinden `compose.yaml`'ı `compose.yaml.new` olarak çıkarır (her zaman deploy edilen imajla aynı);
   - yeni dosyayla `migrate`, ardından `verify` çalıştırır. Biri başarısız olursa çalışan servisler, `compose.yaml` ve `.env` değişmez; yalnız başarılı olmuş bir migration uygulanmış kalır (migration'lar ileri gider; aşağıdaki "Geri alma");
   - yeni compose dosyasını yerine koyar, servisleri başlatır, imajı `.env`'e (`TRADER_IMAGE=...`) ve `deployments.log`'a yazar.

Deploy sonrası kontrol (VPS'te, yönetici kullanıcıyla):

```sh
sudo -u deploy sh -c 'cd /opt/trader && docker compose ps'      # .env imajı taşır
sudo -u deploy sh -c 'cd /opt/trader && docker compose logs --tail 50 observe'
sudo ls -l /var/lib/trader/observe/
tail -n 5 /opt/trader/deployments.log
```

### Geri alma

Actions → önceki başarılı `release` çalıştırması → **Re-run jobs** → `deploy`. Yeniden çalıştırma o çalıştırmanın imaj digest'ini kullanır (yeniden derlemez) ve yine onay ister. Migration'lar yalnız ileri gider: geri alınan sürüm, daha yeni bir migration uygulanmış veritabanında `schema is newer than this binary` ile durur. Migration içeren bir sürümü geri almak gerekirse ileri yönde düzeltme yayınlanır.

## 4. Bakım

- **Taban imajları** (Dockerfile, `compose.yaml`, CI) `etiket@sha256:digest` olarak sabitlenir. Güncellemek için: CI'ın `container` işindeki "base image digests" adımı etiketlerin güncel digest'lerini yazar; Dockerfile `ARG GO_IMAGE` / `ARG RUNTIME_IMAGE`, `compose.yaml` ve iş akışlarındaki `postgres` imajı buna göre güncellenir.
- **Altyapı maliyeti:** ilk Lightsail faturasından sonra aylık VPS + veritabanı toplamını `gates.infra_monthly_usd`'ye yazın (kapı hesabına girer).
- **Lightsail PostgreSQL 16 desteği:** Lightsail ana sürüm sonlarını duyurur; bir sonraki ana sürüme geçiş ayrı bir değişiklik olarak planlanır (kod 16 ve üstünü kabul eder).
