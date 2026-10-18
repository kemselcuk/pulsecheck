# Karar Günlüğü

PulseCheck'te verilen teknik kararların kronolojik kaydı. Her kayıt: **bağlam → karar → gerekçe**.
Bu günlük CV/görüşme kanıtıdır: her kararın *neden* seçildiği buradan izlenebilir.

---

## Kapsam aşamasında verilen kararlar (kullanıcı onaylı, 2026-10-18)

### D1 — Kontrol aralığı: varsayılan 30 sn, hedef başına override
- **Bağlam:** Demo canlı görünürse iyi; ama gerçek izleme ritmi de makul olmalı.
- **Karar:** Varsayılan `30s`; her hedef kendi `interval` alanıyla override edebilir.
- **Gerekçe:** Demo yapılandırması 10 sn'ye düşürerek canlılık gösterir; kod değişmez. Tek sabit değer iki ihtiyacı birden karşılamaz.

### D2 — İstek timeout: varsayılan 5 sn, hedef başına override
- **Bağlam:** Timeout, "geciken hedef diğerlerini engellemez" vaadinin temeli.
- **Karar:** Varsayılan `5s`; istek başına `context.WithTimeout`.
- **Gerekçe:** 2 sn agresif (yavaş ama sağlıklı hedefleri yanlış negatif yapar), 10 sn demoyu uzatır. 5 sn gerçekçi orta değer; demo `/slow` hedefi bunu aşacak şekilde (8 sn) kurulur.

### D3 — Geçmiş saklama: 24 saat, arka planda temizlik
- **Bağlam:** SQLite dosyası sınırsız büyümese de sınırsız geçmiş küçük projede gereksiz.
- **Karar:** 24 saatten eski `results` satırları periyodik (10 dk) silinir.
- **Gerekçe:** Tek parametre, öngörülebilir dosya boyutu; "hedef başına N kayıt" alternatifi silme mantığını karmaşıklaştırırdı.

### D4 — Çakışan kontrol: atla ve `skipped` olarak kaydet
- **Bağlam:** Kontrol süresi aralığı aşarsa (geciken hedef) yeni tur vakti gelir.
- **Karar:** Hedef başına in-flight kilidi; meşgulse tur atlanır ve sonuca `outcome=skipped` + neden yazılır.
- **Gerekçe:** (a) Geciken hedef kuyruk şişirmez, kimseyi bloklamaz; (b) davranış test edilebilir olur; (c) "bekleyeni iptal et" `down` ile `iptal` kavramlarını karıştırır; (d) paralel çalıştırmak aynı hedefe gereksiz yük açar.

### D5 — Karar aktarımı: tek karar günlüğü (bu dosya)
- **Bağlam:** Kullanıcı kararları anlayıp açıklayabilmek istiyor; ADR klasörü / README bölümü / kod turu seçenekleri tartışıldı.
- **Karar:** Tek kronolojik `docs/decisions.md`.
- **Gerekçe:** ADR'den hafif ama aynı bilgiyi taşır; küçük projede klasör yönetimi gereksiz yük.

### D6 — Belge yapısı: merkezi 3 dosya
- **Karar:** `README.md` (kullanım, entegrasyon, kanıt) + `docs/decisions.md` + `docs/PLAN.md`. Görev başına ayrı dosya yok.
- **Gerekçe:** Küçük projede az yerde çok bilgi; iz PLAN.md'de, gerekçe burada, kullanım README'de.

### D7 — Subagent/model dağılımı
- **Bağlam:** Kodlama OpenCode Go üzerinden ajanlarla yapılıyor; kimlikler canlı katalogdan doğrulandı.
- **Karar:** Yazar: `opencode-go/deepseek-v4.1-flash` · Bağımsız reviewer: `opencode-go/glm-5.3-flash` · Orkestratör/kabul: `glm-5.3-flash` (ana oturum) · Nihai onay: kullanıcı.
- **Gerekçe:** İki Flash model en ucuz seçenek; bağımsızlık bağlam düzeyinde sağlanır (ayrı oturum, sıfır paylaşılan bağlam). Daha büyük modellerle review maliyeti küçük projede karşılamaz.

### D8 — Kalite kapısı: kanıt + reviewer + orkestratör
- **Karar:** Her görevde zorunlu kanıt (`go vet` + `go test -race` gerçek çıktısı + işaretli kabul ölçütleri) → bağımsız reviewer → orkestratör kendi doğrulaması → kullanıcıya sentez.
- **Gerekçe:** "Bitti" raporuna tek başına güvenilmez; kanıt zinciri bunu test edilebilir kılar.

### D9 — Kanıt yeri: GitHub Actions + README
- **Karar:** CI: `go vet`, `go test -race`, `go build` her push/PR'da; README'de badge + gerçek çıktı.
- **Gerekçe:** Public repoda ücretsiz; eşzamanlılık iddiaları bağımsız, tekrarlanabilir kanıta bağlanır.

### D10 — Demo videosu: ekran görüntülerinden ffmpeg ile MP4
- **Bağlam:** Yerleşik canlı ekran kayıt aracı yok; ffmpeg kurulu (2026-10-18 doğrulandı).
- **Karar:** Durum ekranından zamanlanmış ekran görüntüleri → ffmpeg ile MP4; ffmpeg yoksa yedek: senaryo + kullanıcı kaydı.
- **Gerekçe:** Video tamamen yerelde üretilir (€0), tekrar üretilebilir script ile.

### D11 — Maliyet: altyapı €0
- **Karar:** Yerel SQLite dosyası, yerel demo hedef sunucusu, GitHub Actions ücretsiz katman. Tek değişken token maliyeti.
- **Gerekçe:** Proje "tek uygulama + tek veritabanı" sınırında; harici servis gerektiren hiçbir tasarım seçilmez.

---

## Uygulama planıyla verilen teknik kararlar (plan onayı, 2026-10-18)

### D12 — Config formatı: JSON
- **Bağlam:** YAML kolay okunur ama `gopkg.in/yaml.v3` bağımlılık ister.
- **Karar:** `encoding/json` ile JSON config.
- **Gerekçe:** Sıfır ek bağımlılık; stdlib testing ile birebir uyum; yapı küçük, okunabilirlik kaybı yok.

### D13 — SQLite sürücüsü: `modernc.org/sqlite`
- **Bağlam:** `mattn/go-sqlite3` CGO ister; `modernc.org/sqlite` saf Go.
- **Karar:** `modernc.org/sqlite`.
- **Gerekçe:** CGO'suz statik derleme → basit, küçük Docker imajı ve sorunsuz çapraz derleme; bu ölçekte performans farkı önemsiz.

### D14 — Demo hedefleri: gömülü `cmd/demo-targets` sunucusu
- **Bağlam:** 200/500/geciken hedeflerin internete bağımlı olması (httpstat.us vb.) demo güvenilirliğini bozar.
- **Karar:** Yalnız stdlib ile küçük bir yerel sunucu: `/ok`→200, `/broken`→500, `/slow`→8 sn gecikme.
- **Gerekçe:** Demo ve testler çevrimdışı, tekrarlanabilir, €0.

### D15 — Entegrasyon hikâyesi: HTTP API üzerinden
- **Bağlam:** "Başka projeye entegrasyon" iki yolla olur: kütüphane import'u veya API.
- **Karar:** Paketler `internal/` altında (dışa kapalı); entegrasyon API + örneklerle (curl, JS fetch).
- **Gerekçe:** "Tek uygulama" sınırını korur; kütüphane yüzeyi istemezsen API sürümleme daha net olur.

### D16 — Git akışı: faz başına dal, görev başına commit
- **Karar:** Kısa ömürlü faz dalları (`faz-0-iskelet` vb.), kabul sonrası `main`'e birleşme; her görev ayrı commit. CI `main` + PR'larda.
- **Gerekçe:** Temel geçmiş okunur; PR geçmişi review kültürünün kanıtı. (Plan metnindeki "görev başına dal"dan faz ölçeğine sadeleştirildi — küçük görevler tek faz dalında ayrı commit olarak izleniyor.)
