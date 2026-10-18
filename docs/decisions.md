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

---

## Uygulama sırasında verilen kararlar

### D17 — Boş string config alanları: varsayılana düş, hata verme
- **Bağlam:** Faz 1 review'ında "minor" bulgu: `server.addr` / `storage.db_path` boş veya yalnız boşluk verilirse hata yerine varsayılana düşüyor; sayısal alanlarda ise açık 0 hata.
- **Karar:** String alanlar için davranış kalır (boş = verilmemiş sayılır); davranış paket dokümanına ve kod içi yoruma yazıldı. Hedef `name` istisna: boş olamaz.
- **Gerekçe:** "Katı hata" alternatifi kullanıcıyı gereksiz cezalandırır; JSON'da alanı hiç yazmamak ile `""` yazmak arasında anlamsal fark yaratmak küçük bir yapılandırmada kafa şişirici. Belirsiz davranış kabul edilemez — o yüzden belgelenerek çözüldü. (Faz 1 review turu, 2026-10-18.)

### D18 — Review bulgularının ayrımı: minor/nit takibi
- **Karar:** Review'da "minor" bulgu ya o turda düzeltilir ya da karar günlüğüne alınır; "nit"ler bilinçli olarak kabul edilir ve aşağıdakiler kayda değer olanlar not edilir: (a) iptal edilen `/slow` isteği logda 200 görünebilir (kozmetik), (b) bind hatası + eşzamanlı sinyal mikrosaniyelik pencerede birbirini maskeler (teori, kabul edildi).
- **Gerekçe:** Küçük projede nit avına girmek teslimatı yavaşlatır; ama her kabul edilen bulgunun gerekçeli kaydı olmalı.

### D19 — Zamanlama testleri invariant iddia eder, çizelge (interleaving) zorlamaz
- **Bağlam:** Faz 2'de "worker pool doygunluğu" dalını test ederken ilk taslak, tam 1 skip bekliyordu; 200 koşumlu stress bunun ~%17 ihtimalle 2 skip olabildiğini gösterdi (worker-vs-tick goroutine planlama yarışı). İlk düzeltme denemesi private alan kurcalayarak çizelgeyi zorladı — reddedildi.
- **Karar:** Test, davranış garantisi olan invariantı iddia eder: toplam kayıt tam 3; doygunluk skip'i 1–2 arası; kabul edilenlerin tamamı up/200; başka outcome yok. Private state erişimi yasak; `Run` startup'ı testin kendisinde kalır. Her iki çizelge de `-race -count=200` ile kanıtlanır.
- **Gerekçe:** Scheduler'ın borcu "kapasite-bounded kabul + taşarsa skip"tir; worker'ın buffer'dan mı yoksa doğrudan kanaldan mı aldığı sözleşmenin parçası değildir. Çizelgeyi zorlayan testler kırılgandır ve CV'de kusur olarak okunur. (Faz 2 takip turu, 2026-10-18.)

### D20 — Shutdown'da iptal edilen kontroller: `error`/"context canceled" olarak kaydedilir
- **Bağlam:** Graceful shutdown in-flight kontrolü context iptaliyle keser; bu sonucun kaydedilip kaydedilmeyeceği belirsizdi.
- **Karar:** Kaydedilir (outcome `error`, Err "context canceled"); kuyu-şekillendirme (filtreleme) gerekiyorsa store/UI katmanında yapılır. Checker bilgiyi gizlemez.
- **Gerekçe:** Kayıt doğrudur (kontrol gerçekten yarıda kaldı); checker'ın içinden filtrelemek bilgi kaybıdır ve test edilebilirliği düşürür. Faz 2 review kararı.

### D21 — Retention'da yanlış yapılandırma: veri yıkımı asla sessiz olmaz
- **Bağlam:** Faz 3 review'ı bir "minor" bulgu sundu: `window <= 0` iken temizleyici tüm tabloyu siliyordu (cutoff ≥ now olur). Üretimde config doğrulaması engelliyor ama programatik kullanım savunmasızdı.
- **Karar:** `window <= 0` → retention tamamen devre dışı + yüksek sesle `Warn`; `deleteOlderThan` da no-op döner (çift emniyet). `period <= 0` zaten tek koşum + Warn ile işliyor — asimetri giderildi. İki yeni test pin'ledi.
- **Gerekçe:** "Silme"nin hata modu hiçbir zaman "hepsini sil" olmamalı; bastırılmış veri kaybı, uyarılı atalemeden daha kötüdür. (Faz 3 review bulgusu, orkestratör yaması, 2026-10-18.)

### D22 — `Record`/`RecordResult` ayrımı
- **Bağlam:** `checker.Recorder` interface'i hata dönüşü içermiyor; store'un hata bildiren yazım fonksiyonu da isteniyordu — Go overloading istemediği için tek imzayla ikisi yapılamaz.
- **Karar:** `Record(Result)` interface'i karşılar (kalıcılık hatasını loglar), `RecordResult(Result) error` hata gereken çağıranlar için. Compile-time assertion: `var _ checker.Recorder = (*Store)(nil)`.
- **Gerekçe:** Faz 2 interface'ini şimdi değiştirmek `MemoryRecorder` + scheduler'ı gereksiz yere sarsar; hata kanalına gerçek bir ihtiyaç doğarsa o zaman değiştirilir. `r.URL`'yi saklamama kararı da burada: hedefler config'den gelir, `target` doğal anahtar — saklasaydık config değişince bayat kopya sorunu doğardı. (Faz 3 review kararı.)

### D23 — Demo portları geliştirici makinesindeki yaygın servislere çarpışmamalı
- **Bağlam:** Bu makinede kullanıcının Docker hizmeti (mongo-express) `127.0.0.1:8081`'i tutuyordu; `demo-targets` IPv6 (::1) tarafına bağlandığından ilk koşum çalıştı ama süreç ölünce istekler IPv4'te Express'e düştü — tüm hedefler `down` oldu. Ek çevresel ders: varsayılan interval (10s) timeout'tan (5s) büyük olunca `skipped` davranışı canlıda asla görünmüyor.
- **Karar:** Demo port `18081` (yaygın geliştirici portlarından uzak); `config.demo.json`'da `demo-slow` hedefi `interval: 3s` override'ı alıyor (timeout 5s > interval → in-flight skip'ler canlıda görünür). Doğrulama testi, hedef başına beklenen intervali pin'liyor.
- **Gerekçe:** "Kolay yerel başlatma" vaadi, kullanıcının makinesinde zaten çalışan servislere çarpmamayı içerir; demo videosu (K10) da `skipped` kayıtlarını görünür kılmak zorunda. (Faz 4 canlı doğrulaması sırasında öğretildi, 2026-10-18.)

### D24 — Shutdown: tek sinyal-context'i tüm yaşam döngüsünü yönetir
- **Bağlam:** Faz 5'te checker, retention ve HTTP sunucusu tek bir `main`'de birleşiyor; kapanışta kimin kimi bekleyeceği belirsizdi.
- **Karar:** `signal.NotifyContext(SIGINT, SIGTERM)` ile üretilen tek context hem checker'ın hem retention'ın context'idir. Sinyal gelince önce HTTP `Shutdown` (5 sn deadline) boşaltılır, sonra checker'ın dönüşü 10 sn hard cap ile beklenir, en son store kapatılır ve "shutdown complete" loglanır. Bekleme zaman aşımı → dürüst hata + `exit 1`.
- **Gerekçe:** "İptal" için ikinci bir mekanizma (ek cancel fonksiyonu) yaratmak iki doğruluk kaynağı doğururdu; context zaten checker'ın iptal yoludur. Hard cap, bir hata durumunda sürecin sonsuza kadar asılı kalmasını yasaklar — "asla asılı kalma" vaadi test edilebilir bir çıkış koduyla garanti edilir. (Faz 5 implementasyonu, 2026-10-18.)

### D25 — Docker: tek Dockerfile, iki binary, iki compose servisi
- **Bağlam:** Servis ve demo hedefi ayrı programlar; "iki imaj mı, tek imaj mı" sorusu açıktı.
- **Karar:** Tek çok aşamalı `Dockerfile` iki statik binary üretir (`/pulsecheck`, `/demo-targets`); tek son imaj `ENTRYPOINT ["/pulsecheck"]` taşır. Compose'da `demo-targets` servisi `entrypoint: ["/demo-targets"]` ile override eder; `pulsecheck` servisi ise `command: ["-config", "..."]` ile yalnızca argüman ekler.
- **Gerekçe:** İki imaj imaj yönetimini ve build süresini ikiye katlar; tek imaj + iki entrypoint daha küçük ve yeterli. Docker `command`'ı `ENTRYPOINT`'ten SONRA eklediği için compose'da binary adını tekrar yazmak (görev metnindeki `command: /pulsecheck -config ...` biçimi) süreci `/pulsecheck /pulsecheck -config ...` yapardı; bu yüzden `command` yalnız argümanları taşır. (Faz 5, görev metninden bilinçli sapma, 2026-10-18.)

### D26 — Runtime imajı: scratch değil alpine
- **Bağlam:** Statik binary ile `scratch` en küçük imajı verir.
- **Karar:** Son imaj `alpine:3.21` + `ca-certificates`, root olmayan `pulsecheck` kullanıcısı.
- **Gerekçe:** `scratch` ne `ca-certificates` ne de bir kabuk içerir; `https://` hedefler (örnek config'de var) sessizce `x509` hatası verirdi ve hata ayıklama imkânsız olurdu. Kazanılan birkaç MB, gerçek dünyada çalışan bir imajın yanında önemsiz. (Faz 5, 2026-10-18.)

### D27 — Bilinen kabul edilmiş davranışlar: hata yolları ve ikinci sinyal
- **Bağlam:** Faz 5 review'ı iki "minor" sundu: (1) bind hatası / Shutdown hatası yollarında süreç, checker'ı beklemeden döner — `st.Close()` in-flight yazımla yarışabilir; (2) shutdown sırasında ikinci SIGINT yutulur, zorla çıkış yok.
- **Karar:** İkisi de bilinçli olarak kabul edildi, düzeltilmedi. (1) için: `database/sql`'ın `Close`'u başlamış sorguların bitmesini bekler ve süreç zaten hatayla çıkıyor; happy path'te tam "drain-sonra-kapat" disiplini korunur. (2) için: 10 sn'lik checker hard-cap'i toplam yanıt vermezliği ~15 sn ile sınırlar; klasik "ikinci sinyal = zorla çıkış" davranışı gelecekte küçük bir `stop()` kaydırmasıyla eklenebilir.
- **Gerekçe:** Küçük projede gözlemlenemeyen yarışlar için refactor, kapsam disiplinini bozar; davranışın belgelenmiş olması "gizli kusur" ile "bilinçli sınırlama"yı ayırır. (Faz 5 review, 2026-10-18.)
