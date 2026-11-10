# Kullanıcı görüşme kiti (M2 öncesi)

**Amaç:** Web paneline (M2) yatırım yapmadan önce, insanların yedeklerinin geri yüklenebilir
olduğunu bugün nasıl doğruladıklarını ve hangi kanıta ihtiyaç duyduklarını öğrenmek.
Ürünü satmak değil, gerçek davranışı anlamak. (English summary at the end.)

## Kimlerle (8–10 görüşme)
- 3–4 self-hosted geliştirici / küçük ekip (Coolify, Dokploy, Docker Compose)
- 2–3 DevOps / sistem yöneticisi (birden fazla sunucu)
- 2–3 küçük/orta SaaS şirketinden teknik sorumlu (müşteri veya denetim baskısı olanlar)

Uygunluk: Son 12 ayda kendi sunucusunda PostgreSQL veya Docker volume'ü yedekleyen kişi.

## Görüşme (30 dk, kayıt izinle)

**Isınma (3 dk)**
1. Hangi sistemleri işletiyorsunuz? Kaç sunucu, kaç veritabanı, hangi araçlar (Coolify, Dokploy, Compose, Kubernetes)?

**Bugünkü yedekleme (7 dk)**
2. Yedeklerinizi nasıl alıyorsunuz? Hangi araç, ne sıklıkla, nereye?
3. Bir yedeğin "başarılı" olduğunu nasıl anlıyorsunuz?
4. Son yedek ne zaman alındı ve bunu nereden biliyorsunuz?

**Geri yükleme gerçeği (10 dk) — en önemli bölüm**
5. En son ne zaman gerçek bir geri yükleme yaptınız? Anlatır mısınız: ne oldu, ne kadar sürdü?
6. Geri yüklemeyi hiç *test* amaçlı yapıyor musunuz? Ne sıklıkla, kim, nasıl? Yapmıyorsanız neden?
7. Geri yüklemede bir şeyin ters gittiği bir an oldu mu? (eksik veri, uyumsuz sürüm, eksik eklenti, bozuk dosya…)
8. Bir yedeğin gerçekten çalıştığından ne kadar eminsiniz (1–10)? Neden o sayı?

**Kanıt ve sorumluluk (5 dk)**
9. Müşteri, yönetici veya denetçi size "kurtarabildiğinizi kanıtlayın" diye sordu mu? Ne istediler?
10. Kurtarma süresi (RTO) veya veri kaybı (RPO) hedefiniz var mı? Nasıl ölçüyorsunuz?

**Çözüm tepkisi (5 dk)** — en sonda, kısa demo veya rapor örneği gösterilerek
11. (Örnek VERIFIED raporunu göster) Bu rapor size ne söylüyor? Neyi eksik buluyorsunuz?
12. Bunu nerede görmek isterdiniz: terminal, e-posta/Slack, web paneli, Coolify/Dokploy içinde?
13. Bu her gece otomatik çalışsaydı, hangi koşulda size haber vermesini isterdiniz?
14. Bunun için para öder miydiniz / şirketiniz öder miydi? Hangi kısmı için (yönetilen panel, destek, denetim raporu)?

**Kapanış**
15. Başka kiminle konuşmalıyım?

## Görüşmeciye notlar
- Varsayım değil, **geçmiş davranış** sorun ("en son ne zaman…").
- "Kullanır mıydınız?" sorusuna verilen evetler zayıf kanıttır; gerçek olaylar ve harcanan zaman güçlü kanıttır.
- Ürünü 11. sorudan önce anlatmayın.

## Not şablonu (her görüşme için)
| Alan | Not |
|---|---|
| Profil (rol, ölçek, araçlar) | |
| Yedekleme yöntemi | |
| Son gerçek geri yükleme + sonuç | |
| Test sıklığı (hiç / yıllık / aylık / otomatik) | |
| Güven puanı (1–10) | |
| Kanıt ihtiyacı (kim istiyor, ne biçimde) | |
| İstenen arayüz (CLI / bildirim / panel / Coolify-Dokploy) | |
| Ödeme isteği + neye | |
| Önemli alıntılar | |

## Kısa anket (Google Forms / Tally için, 2 dk)
1. Rolünüz? (geliştirici / DevOps-SRE / sistem yöneticisi / CTO-teknik lider / diğer)
2. Neyi yedekliyorsunuz? (PostgreSQL / MySQL-MariaDB / MongoDB / Docker volume / dosyalar / diğer) — çoklu
3. Hangi araçla? (Coolify / Dokploy / pg_dump+cron / restic / Kopia / Borg / pgBackRest / bulut sağlayıcı / diğer) — çoklu
4. Son 12 ayda geri yüklemeyi kaç kez test ettiniz? (hiç / 1 / 2–5 / 6+ / otomatik)
5. Bir yedeğiniz gerektiğinde geri yüklenemedi mi? (evet / hayır / bilmiyorum)
6. Yedeklerinizin çalıştığından ne kadar eminsiniz? (1–10)
7. Otomatik "geri yükleyip doğrulama" size ne kadar değerli? (1–5)
8. Sonucu nerede görmek istersiniz? (CLI / e-posta / Slack-Discord / web paneli / Coolify-Dokploy içinde) — çoklu
9. 15–30 dk görüşmeye açık mısınız? (e-posta, isteğe bağlı)

## Karar kuralları (görüşmeler sonrası)
- Görüşülenlerin ≥%60'ı web paneli isterse → M2 öncelikli kalır; çoğu Coolify/Dokploy içinde isterse → M3'teki entegrasyonlar öne alınır.
- ≥%50'si MySQL/MariaDB isterse → bir sonraki veritabanı desteği o olur.
- Denetim/kanıt ihtiyacı belirginse → PDF/imzalı rapor (M3) öne alınır.

---

### English summary
Interview 8–10 self-hosters / DevOps / SaaS tech leads (30 min). Focus on *past behaviour*: last
real restore, restore-testing frequency, failures, confidence (1–10), who asks for recovery
evidence and in what form. Show a sample VERIFIED report only at the end and ask where they want
to see it (CLI, notifications, web panel, inside Coolify/Dokploy) and what they would pay for.
Decision rules above decide whether M2 (web panel) or M3 integrations come first.
