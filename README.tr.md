# Unth Recover

> **Yedeğiniz, kurtarmanız değildir.** Sadece yedek almayın — kurtarabildiğinizi kanıtlayın.

Unth Recover, **Unmade Things** tarafından geliştirilen açık kaynak bir felaket kurtarma
aracıdır. PostgreSQL veritabanlarını yedekler ve her yedeğin gerçekten geri
yüklenebildiğini **kanıtlar**: yedeği yepyeni, izole bir PostgreSQL konteynerine geri
yükler; verileri, şemayı ve kısıtları, kaynaktan *dump ile aynı snapshot içinde* alınan
parmak iziyle karşılaştırır. Ancak bundan sonra yedek `VERIFIED` olur.

[English README](README.md) · Lisans: [Apache-2.0](LICENSE)

> **Durum: `v0.1.0-alpha` (M0 — PostgreSQL kurtarma kanıtı).** Ön sürüm yazılımıdır,
> **production kullanımına hazır değildir.**

## Hangi problemi çözer?

Çoğu yedekleme aracı yalnızca yedeğin *oluşturulduğunu* söyler. Bir dosyanın var olması —
geçerli bir checksum'ı olsa bile — geri yüklenebileceğini göstermez: dump eksik olabilir,
hedefte bir eklenti bulunmayabilir, bir kısıt yükleme sırasında başarısız olabilir.
Ekipler bunu genellikle bir olay anında öğrenir.

## Farkı nedir?

- Her doğrulama, **yeni ve izole** bir konteynerde **gerçek** bir geri yükleme yapar
  (internal ağ, yayınlanmış port yok, kaynak limitleri, tüm capability'ler düşürülmüş).
- Geri yüklenen veri, kaynak snapshot'ıyla karşılaştırılır: kesin satır sayıları, tablo
  içerik hash'leri, şema nesneleri, kısıtlar (PK/FK/UNIQUE/CHECK/EXCLUDE), sequence'lar ve
  sizin tanımladığınız SQL doğrulama sorguları.
- Rapor, neyin doğrulanmadığını da açıkça belirtir (`verification_scope`).
- Gözlemlenen test restore süresi ile RTO; tahmini veri kaybı penceresi ile RPO birbirinden ayrılır.

**Temel kural kodla ve testlerle uygulanır:** gerçek restore yapılmadan ve tüm zorunlu
kontroller geçmeden hiçbir yedek `VERIFIED` olamaz. Checksum tek başına yeterli değildir.

## Kurulum

Gereksinimler: Linux, Docker Engine, sunucu sürümünüze eşit veya daha yeni PostgreSQL
istemci araçları (`pg_dump`, `pg_restore`), derlemek için Go 1.26+.

```bash
git clone https://github.com/unmadethings/unthrecover.git
```

```bash
cd unthrecover && make build && sudo install bin/unth /usr/local/bin/unth
```

```bash
unth doctor
```

## Kullanım

```bash
export SHOP_DB_PASSWORD='…'          # parolayı asla config dosyasına yazmayın
unth discover postgres --config examples/backup.yaml
unth backup postgres --config examples/backup.yaml
unth backup list
unth verify <backup-id>              # çıkış kodu 0 = VERIFIED, 3 = doğrulanamadı
unth report <backup-id> --format json
```

Ayrıntılı rehber: [docs/tr/HIZLI_BASLANGIC.md](docs/tr/HIZLI_BASLANGIC.md) ·
Komut referansı (İngilizce): [docs/CLI_REFERENCE.md](docs/CLI_REFERENCE.md)

## Desteklenenler (M0)

- PostgreSQL 12 – (`pg_dump` ana sürümünüz) mantıksal yedek; **PostgreSQL 16 ile test edildi**
- Yerel depo (`--data-dir`), SQLite metadata, versiyonlu migration, denetim kaydı
- Docker'da izole restore testi, JSON ve metin raporları
- Docker ve PostgreSQL için salt-okunur keşif
- Kesintiye uğramış işlemlerin kurtarılması (`unth cleanup`)

## Henüz geliştirilmeyenler

Docker volume yedeği, S3 uyumlu depolama, şifreleme, imzalı manifest, zamanlama,
saklama politikaları, web arayüzü, production restore, çoklu sunucu.

## Bilinen sınırlamalar

- Yalnızca Linux.
- Yedekler **şifreli değildir**; checksum **imza değildir**. Veri dizinini koruyun.
- Roller, sahiplik, yetkiler ve küme ayarları tek veritabanı dump'ına dahil değildir ve doğrulanmaz.
- Docker soketine erişim root yetkisine eşdeğerdir.

## Katkı ve sponsorluk

Katkı için [CONTRIBUTING.md](CONTRIBUTING.md); güvenlik açıkları için [SECURITY.md](SECURITY.md).
Unmade Things için GitHub Sponsors henüz etkin değildir; şimdilik en iyi destek
aracı denemek, hata bildirmek ve bugün kurtarma testlerini nasıl yaptığınızı paylaşmaktır.

## Lisans

Apache License 2.0.
