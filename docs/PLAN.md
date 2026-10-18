# PulseCheck — Uygulama Planı

**Durum:** Onay bekliyor · **Tarih:** 2026-10-18
**Sorumlu düzeni:** Kullanıcı = nihai kapsam ve uygulama onayı. Orkestratör (ben) = proje sorumlusu ve teknik kabul. Kod = `opencode-go/deepseek-v4.1-flash`, bağımsız review = `opencode-go/glm-5.3-flash`.

---

## 1. Kapsam

**Hedef:** Yapılandırma dosyasındaki HTTP hedeflerini belirli aralıklarla, sınırlı worker pool ile eşzamanlı kontrol eden; sonuçları SQLite'ta saklayan; küçük bir API + tek sayfalık durum ekranı sunan; graceful shutdown yapan tek Go uygulaması.

**Kapsam dışı:** Dağıtık scheduler, alarm/bildirim platformu, kimlik doğrulama, çok kullanıcılılık, veritabanı sunucusu.

### Kilitlemiş kararlar (kullanıcı onaylı)

| # | Karar | Değer |
|---|-------|-------|
| K1 | Kontrol aralığı | Varsayılan **30 sn**, hedef başına override |
| K2 | İstek timeout | Varsayılan **5 sn**, hedef başına override |
| K3 | Geçmiş saklama | **24 saat**, arka planda periyodik temizlik |
| K4 | Çakışan kontrol | Aynı hedefte önceki kontrol sürerken yeni tur **atlanır**, sonuca `skipped` + neden olarak yazılır (hedef başına kilit) |
| K5 | Öğrenme yöntemi | Karar günlüğü: `docs/decisions.md` (her anlamlı karar: bağlam → karar → gerekçe) |
| K6 | Belge yapısı | Merkezi 3 dosya: `README.md`, `docs/decisions.md`, `docs/PLAN.md` |
| K7 | Subagent/model | Yazar: `deepseek-v4.1-flash` · Denetleyen: `glm-5.3-flash` (bağımsız oturum) · Orkestratör: `glm-5.3-flash` |
| K8 | Kalite kapısı | Zorlu kanıt (`go vet` + `go test -race` çıktısı + işaretli kabul ölçütleri) → reviewer → ben → kullanıcı |
| K9 | Kanıt yeri | GitHub Actions (ücretsiz) + README'de gerçek çıktı/badge |
| K10 | Demo | Durum ekranı ekran görüntüleri + **ffmpeg ile MP4** (ffmpeg yoksa: senaryo + kullanıcı kaydı) |
| K11 | Maliyet | Altyapı **€0**; tek değişken token maliyeti (Flash modeller) |

### Plan onayında netleşecek teknik kararlar (planla birlikte onaya sunulur)

| # | Öneri | Gerekçe |
|---|-------|---------|
| T1 | Config formatı **JSON** (`encoding/json`) | Sıfır ek bağımlılık; stack listesiyle uyumlu |
| T2 | SQLite sürücüsü **`modernc.org/sqlite`** (saf Go, CGO'suz) | Statik derleme + basit Docker; `mattn/go-sqlite3` CGO gerektirir |
| T3 | Demo hedefleri için gömülü **`cmd/demo-targets`** sunucusu (yalnız stdlib) | 200 / 500 / geciken hedefler tamamen yerelde, ağ bağımlılığı yok |
| T4 | Entegrasyon hikâyesi **HTTP API üzerinden** (`internal/` paketleri dışa açık kütüphane değildir) | "Tek uygulama" sınırını korur; README'de curl + fetch örnekleri |
| T5 | Git akışı: görev başına kısa ömürlü dal → kabul sonrası `main`'e birleşme; CI `main` + PR'larda | PR geçmişi CV kanıtı; istenirse doğrudan commit'e düşer |

---

## 2. Çalışma düzeni (her görev için aynı akış)

1. **Delegasyon:** Görev tanımı + kabul ölçütleri `delegate_task` ile `deepseek-v4.1-flash`'e (implementer).
2. **Kanıt zorunlu:** Implementer çıktısında `go vet ./...` ve `go test -race ./...` gerçek çıktısı + işaretli kabul ölçüt listesi olur.
3. **Bağımsız review:** Aynı diff + kanıt, `glm-5.3-flash`'e salt okunur reviewer olarak gider.
4. **Orkestratör kabulü:** Ben kendim testleri koşar, davranışı çalıştırarak doğrularım; "bitti" raporuna tek başına güvenmem.
5. **Kullanıcı sentezi:** Görev sonunda kısa kabul özeti + kanıt bağlantısı. Dönüm noktalarında (faz sonu) onayın alınır.
6. `docs/decisions.md` o görevde verilen teknik kararlarla güncellenir.

---

## 3. Fazlar ve görevler

> Görevler bilinçli küçük: her biri tek bir delegelik iş, net kabul ölçütlü.

### Faz 0 — İskelet ve kanal

> **Durum:** ✅ Tamamlandı (2026-10-18) — commit'ler `b5bf8c1`, `60fc004`, `1db5081`. Implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE); orkestratör doğrulaması: build/vet/test -race ve smoke test yeniden koşuldu, exit 0.

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T0.1 Karar günlüğü** | `docs/decisions.md` başlangıç içeriği: K1–K11 + T1–T5 kayıtları | Her kayıtta bağlam/karar/gerekçe alanı var; kullanıcı seçimleriyle birebir uyumlu |
| **T0.2 İskelet** | `go.mod`, klasör yapısı: `cmd/pulsecheck`, `cmd/demo-targets`, `internal/{config,checker,store,api,web}`, `Makefile`, `.gitignore`, boş paketler derlenebilir | `go build ./...` ve `make test` hatasız; dizin yapısı README'de tasvir edilecek kadar açık |
| **T0.3 CI** | `.github/workflows/ci.yml`: `go vet`, `go test -race`, `go build` | Push'ta yeşil check; badge README'ye eklenecek durumda |

### Faz 1 — Yapılandırma

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T1.1 Config yükleyici** | JSON config: `targets[]` (name, url, opsiyonel interval/timeout), `defaults` (interval=30s, timeout=5s, workers=4), `storage` (db_path, retention=24h), `server` (addr) | Testler: geçerli dosya okunur; **varsayılanlar uygulanır** (30s/5s/24h/4); geçersiz URL/boş name reddedilir; hedef override'ı kazanır |
| **T1.2 Örnek yapılandırmalar** | `config.example.json` (gerçekçi hedefler) + `config.demo.json` (yalnız yerel demo hedefleri) + `cmd/demo-targets` (stdlib sunucu: `/ok`→200, `/broken`→500, `/slow`→8 sn gecikme) | `demo-targets` yerelde üç davranışı da verir; `config.demo.json` bu adreslere işaret eder; test bunları doğrular |

### Faz 2 — Checker: HTTP + eşzamanlılık

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T2.1 Tek kontrol** | `net/http` istek: istek başına `context.WithTimeout`, süre ölçümü, sınıflandırma: `up / down / timeout / error` (+status code, hata nedeni) | httptest ile: 200→`up`, 500→`down`, timeout→`timeout`, bağlantı hatası→`error`; süre kaydı doğru; goroutine sızıntısı yok |
| **T2.2 Worker pool + scheduler** | Sabit N worker, tur bazlı kuyruk; **hedef başına in-flight kilidi** (K4: meşgulse tur `skipped` olarak kaydedilir); `SIGINT/SIGTERM` → scheduler durur, in-flight kontroller context ile iptal, timeout'lu drain, store kapanır | Testler: (a) sunucu tarafında ölçülen **eşzamanlılık ≤ workers**; (b) **geciken hedef diğerlerini bloklamaz** — slow hedef sürerken diğer hedefler aynı turda kayda geçer; (c) kontrol süresi > interval iken `skipped` kaydı + nedeni oluşur; (d) shutdown: iptal edilen kontrol `context` hatasıyla temiz biter, `runtime.NumGoroutine` başlangıca döner |
| **T2.3 Race doğrulaması** | Faz 2 testlerinin tamamı `-race` ile | `go test -race ./internal/checker/...` yeşil; çıktı README kanıtına hazır |

### Faz 3 — SQLite saklama

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T3.1 Şema + store** | `results` tablosu: `id, target, started_at, outcome, status_code, duration_ms, error`; `(target, started_at)` indeksi; WAL modu; ekleme + sorgulama fonksiyonları | Testler: yaz/oku gidiş-dönüş; `skipped` dahil tüm outcome'lar saklanır; eşzamanlı yazımda `-race` temiz |
| **T3.2 Retention** | Arka plan temizleyici (örn. 10 dk'da bir): 24 saatten eski satırları siler; shutdown'ta düzgün durur | Test: 25 saatlik kayıtlar silinir, yeniler kalır; temizleyici iptal edilebilir |

### Faz 4 — API + durum ekranı

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T4.1 JSON API** | `GET /api/status` (hedef başına son durum + 24 sa özet), `GET /api/history/{name}?limit=N`; yalnız stdlib `net/http` (Go 1.22 pattern routing), doğru hata kodları | httptest: `/api/status` şeması doğru; bilinmeyen hedef → 404; `limit` çalışır; hatalı yol → 404/405 |
| **T4.2 Durum ekranı** | Tek HTML sayfası (`embed.FS`), bağımlılıksız vanilla JS, 5 sn'de bir `/api/status` polling; hedef başına durum rozeti + süre + son N sonucun renkli göstergesi | Test: sayfa 200 + beklenen elemanlar; JS/HTML'de harici kaynak yok (offline çalışır); README ekran görüntüsü buradan alınacak |

### Faz 5 — Bütünleştirme, Docker, README

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T5.1 main + graceful shutdown** | Tüm parçalar birleşir; `slog` ile yapılandırılmış log; `http.Server.Shutdown` + checker drain + store close sırası | Uçtan uca: `make run` ile üç demo hedefi panelde görünür; `Ctrl+C` sonrası süreç temiz çıkar, loglar shutdown adımlarını gösterir |
| **T5.2 Docker** | Çok aşamalı `Dockerfile` (CGO=0 statik binary, minimal imaj) + `docker-compose.yml` (app + demo-targets) | `docker compose up` ile tam demo çalışır; imaj derlenir |
| **T5.3 README** | Quickstart (yerel + Docker), config referansı, API dokümanı + **entegrasyon örnekleri** (curl, JS fetch), kararlar özeti → `docs/decisions.md`, CI badge + gerçek `go test -race` çıktısı, demo gömülü | Taze klon → kopyala-yapıştır komutlarla çalışır; entegrasyon örneği çalışan API'ye karşı doğrulanmış |

### Faz 6 — Demo videosu

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T6.1 ffmpeg keşfi** | `ffmpeg -version` kontrolü; yoksa kurulum önerisi veya yedeğe geç | Kurulum **öncesi** netleştirildi (sürpriz yok) |
| **T6.2 Video üretimi** | Demo config + durum ekranı; zamanlanmış ekran görüntüleri → ffmpeg ile MP4; alternatif: kullanıcı 45–60 sn ekran kaydı alır (senaryo + adımlar bizden) | 45–90 sn MP4, ≤ ~10 MB, README'de gömülü; **videoda görünen:** 200 dönen hedef, 500 dönen hedef, timeout'a düşen geciken hedef; geciken hedef sürerken diğer hedeflerin güncellenmeye devam ettiği (bloklamadığı) ve `skipped` kayıtlarının göründüğü an |

### Faz 7 — Son kabul

| Görev | İçerik | Kabul ölçütleri |
|---|---|---|
| **T7.1 Full review** | Reviewer + orkestratör tüm repo üzerinden son tur; kabul ölçüt listesinin tamamı işaretlenir | Tüm testler `-race` yeşil; CI yeşil; README/decisions/PLAN güncel; kullanıcı nihai onayı verir |

---

## 4. Demo kanıt matrisi (izlenebilirlik)

| Vaat | Kanıt |
|---|---|
| 200 / 500 / geciken hedefler | `config.demo.json` + `cmd/demo-targets` + demo videosu |
| Geciken hedef bloklamaz | T2.2(b) testi + videoda canlı görünüm |
| Eşzamanlılık sınırı | T2.2(a) testi (`-race`) + CI |
| Graceful shutdown | T2.2(d) testi + T5.1 uçtan uca gözlem |
| Saklama/temizlik | T3.1–T3.2 testleri |
| Entegrasyon | README'de doğrulanmış API örnekleri |
| Test kültürü | CI badge + README'de gerçek çıktı |

## 5. Maliyet

- **Altyapı: €0.** Yerel SQLite dosyası, yerel demo hedefleri, GitHub Actions ücretsiz katman, ffmpeg yerel.
- **Token (tek değişken):** 7 faz × ~2–3 delegelik iş, Flash modellerle kaba tahmin toplamda birkaç € altında. Her görev sonunda gerçek token kullanımı sentezlerde raporlanır; sürpriz olursa durup söz ederim.

## 6. Teslim tanımı (bitiş kriteri)

Yukarıdaki tüm kabul ölçütleri işaretli + demo videosu README'de + CI yeşil + kullanıcı onayı. `docs/PLAN.md` ilerledikçe görev göstergeleriyle güncellenir (☐ → ✅).
