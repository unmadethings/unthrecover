# Hızlı başlangıç (geçici demo veritabanı ile)

Bu rehber yalnızca sentetik veri ve sizin oluşturduğunuz kaynakları kullanır. Aynı
"veri yükle → yedekle → doğrula" akışı M0 hazırlanırken gerçekten çalıştırıldı.

## 1. Demo kaynak veritabanını başlatın
```bash
openssl rand -hex 16 > demo.pass && chmod 600 demo.pass
```

```bash
docker run -d --name unth-demo-src -e POSTGRES_PASSWORD="$(cat demo.pass)" -e POSTGRES_DB=shop -p 127.0.0.1:55432:5432 postgres:16
```

```bash
PGPASSWORD="$(cat demo.pass)" psql -h 127.0.0.1 -p 55432 -U postgres -d shop -f tests/fixtures/seed.sql
```

## 2. İş tanımı (demo.yaml)
```yaml
version: 1
source:
  name: shop-db
  host: 127.0.0.1
  port: 55432
  database: shop
  user: postgres
  password_file: ./demo.pass     # parola asla bu dosyaya yazılmaz
verification:
  queries:
    - name: paid_order_total
      sql: SELECT sum(total_cents) FROM orders WHERE status = 'paid'
```

## 3. Kontrol, yedek, doğrulama
```bash
unth doctor
unth discover postgres --config demo.yaml
unth backup postgres --config demo.yaml
unth verify <backup-id>
```

`verify` her çalıştırmada yeni ve izole bir PostgreSQL konteyneri oluşturur, yedeği gerçekten
geri yükler, satır sayılarını, tablo içerik hash'lerini, şema nesnelerini, kısıtları ve
sorgularınızı kaynak snapshot'ıyla karşılaştırır ve oluşturduğu tüm kaynakları siler.
Çıkış kodu `0` = `VERIFIED`, `3` = doğrulanamadı.

## 4. Raporlar
```bash
unth backup list
unth report <backup-id> --format json
unth audit list
```

Rapordaki **gözlemlenen restore süresi bir RTO garantisi değildir**; **tahmini veri kaybı
penceresi de doğrulanmış bir RPO değildir.** Rapor, neyin doğrulanmadığını da listeler.

## 5. Temizlik
```bash
docker rm -f -v unth-demo-src
```

Beklenmedik bir kesintiden (ör. SIGKILL) sonra kalan doğrulama kaynakları için:
```bash
unth cleanup --dry-run
```
