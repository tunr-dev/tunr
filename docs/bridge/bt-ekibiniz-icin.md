# BT ekibiniz için: AI tedarikçiniz neden tunr Bridge kullanıyor

English: [for-your-it-team.md](for-your-it-team.md)

Bir AI tedarikçisi, ajanının ağınızdaki bir sistemden veri okumasını istiyor:
ERP, muhasebe veritabanı ya da bir dosya sunucusu. Sizden **tunr Bridge**
kurmanızı istedi. Bu sayfa, BT ve güvenlik ekibinizin "evet" demeden önce
soracağı soruları yanıtlıyor. Teknik ayrıntılar ve testler:
[security.md](security.md).

## Tek paragrafta ne olduğu

Ağınızın içindeki bir makinede küçük bir program çalıştırırsınız: **connector**.
Bu program tunr relay'ine HTTPS ile *dışarı doğru* bağlanır; içeri port
açmazsınız, VPN kurmazsınız. Hangi sistemlere erişebileceğini siz söylersiniz.
Tedarikçinin ajanı bunlardan birine erişim *ister*; gerekçesini yazar, süresi
sınırlıdır. Sizin tarafınızda biri onaylamadan hiçbir veri akmaz. Her isteği,
her bağlantıyı ve her sorguyu görürsünüz; erişimi saniyeler içinde
kesebilirsiniz.

## Kurmadan önce tedarikçiden bu cevapları alın

| Sorun | Neden |
| --- | --- |
| Hangi sistem, hangi tablolar, okuma mı yazma mı? | Her kaynağa adı siz verirsiniz; tedarikçi yalnızca bu adları görür. |
| Hangi ajan, onların tarafında hangi kişi adına? | Her grant ajanı ve adına çalıştığı kişiyi gösterir; o kişi tedarikçiden ayrılırsa grant durur. |
| Ne kadar süre, ne sıklıkla? | Grant'ler süresi dolunca kendiliğinden biter (varsayılan üst sınır 8 saat, siz belirlersiniz). |
| Veriyle sonra ne yapacaklar? | Bridge neyin ve ne kadarının okunacağını sınırlar, verinin sonra nasıl kullanılacağını değil. Bu bir sözleşme konusu. |
| Bir terslik olursa kimi arayacaksınız? | Bir muhatap belirleyin. Önce erişimi kesip sonra sorabilirsiniz. |

**Sonra birlikte deneyin:** tedarikçiden üç şey göstermesini isteyin. Onaylı
grant'le çalışan bir sorgu, grant olmadan reddedilen bir sorgu ve grant olsa
bile reddedilen bir yazma denemesi. Üçü de etkinlik kaydınızda görünür.

## Nasıl çalışıyor

```
 tedarikçinin ajanı ──► tunr access ──(TLS 1.3, connector'ınıza sabitlenmiş)──► connector ──► veritabanınız
 (onların bulutu)                  relay yalnızca şifreli veriyi iletir         (sizin ağınız,  (salt okunur login)
                                                                               yalnız dışarı)
```

- **Yalnızca dışarı bağlantı.** Connector relay'e 443 portundan bağlanır.
  Relay adresine çıkışa izin vermeniz yeterli; ağınıza dışarıdan hiçbir şey
  ulaşmaz.
- **Adresler sizde kalır.** Veritabanının adresi ve şifresi connector'ın yerel
  yapılandırmasında ve gizli deposunda durur. Relay ve tedarikçi yalnızca
  `acme/erp-db` gibi bir ad görür.
- **Ajan veritabanı şifresini hiç görmez.** Connector, sizin oluşturduğunuz
  salt okunur bir login'le bağlanır (`tunr connect pg-role` ve `mssql-role`,
  DBA'nız için SQL'i hazırlar) ve her sorguyu kendisi çalıştırır ya da süzer.
- **Üç katmanlı salt okunurluk.** Login yalnızca `SELECT` yapabilir; her oturum
  salt okunurdur (Postgres) ya da her sorgu geri alınır (SQL Server); bir sorgu
  filtresi yazan her şeyi reddeder. `tunr connect resource verify` login'i
  kontrol eder, yazma yetkisi varsa söyler.
- **Hassas kolonlar** sonuçlarda maskelenebilir (Postgres) ya da SQL Server'ın
  kendi Dynamic Data Masking özelliğiyle gizlenebilir. Maskeleme değeri
  gösterilmez kılar ama kararlı bir ajanın `WHERE` ile tahmin denemesini
  durdurmaz. Gerçekten gizli kalması gereken kolonları login'e hiç vermeyin.

## Relay (tunr) neyi görür, neyi göremez

- Yoldaki verinizi **okuyamaz**: akışlar tedarikçinin makinesiyle connector'ınız
  arasında uçtan uca şifrelidir.
- Kimin neyi ne zaman istediğini ve her sorgunun özetini (komut, tablolar,
  satır sayısı, süre) **görür**. Denetim kaydınız budur. Sorgu *metni* ancak
  siz açarsanız saklanır.
- **Ele geçirilmiş bir relay**, tedarikçiye kendini connector'ınız gibi
  göstermeyi deneyebilir. Connector'ınızın sertifika parmak izini
  (`tunr connect status`) tedarikçiye güvendiğiniz bir kanaldan iletin. Relay'i
  kendiniz barındırırsanız üçüncü taraf ortadan kalkar.

## Erişimi kesmek

| Kapsam | Nasıl |
| --- | --- |
| Tek grant | Portalda **Revoke**. Açık bağlantılar kapanır (ölçülen süre ~4 ms). |
| Tek site | **Tüm erişimi kes**. Sitedeki bütün grant'ler iptal olur, bekleyenler reddedilir, otomatik onay kapanır. |
| Relay olmadan | Connector makinesinde `sudo tunr connect kill`. Yeniden başlatmada da kapalı kalır, `tunr connect resume` ile açılır. |
| Kalıcı olarak | `sudo tunr connect uninstall`, ardından güvenlik duvarında relay adresini engelleyin. Portalda tedarikçi bağlantısını iptal edin. |

## Elinizde kalan kayıtlar

**Activity** ekranı her oturumu ve her sorgu özetini gösterir; reddedilen
sorgular işaretlidir. Denetim kaydını kendi arşiviniz ya da KVKK dosyanız için
CSV veya JSON olarak dışa aktarabilirsiniz. Sorgu özeti ajanın ne çalıştırdığını
kaydeder, tedarikçinin sonuçla ne yaptığını değil.

## Sık sorulanlar

**Neden VPN ya da açık port değil?** İkisi de tedarikçiye yerel ağınıza giden
bir yol açar. Connector ise onaylamanızdan sonra, sınırlı bir süre için, salt
okunur olarak, tek bir adlandırılmış sisteme erişim verir.

**Connector neye ihtiyaç duyar?** Veritabanına ulaşabilen ve dışarı HTTPS
bağlantısı kurabilen bir makineye. Genellikle bir Linux sunucu seçilir; orada
sıkılaştırılmış bir systemd servisi olarak çalışır. Mac'te de çalışır. Tek
parça, statik bir programdır.

**Tedarikçi kendi erişimini onaylayabilir mi?** Hayır. Onayı sizin
organizasyonunuzdaki approver rolündeki kişiler verir. Ham TCP kaynaklarına
okuma-yazma erişimi iki farklı onaycı ister. Otomatik onay ancak siz açarsanız,
tek bir tedarikçi için, salt okunur ve sizin belirlediğiniz süre sınırı içinde
çalışır.

**Açık kaynak mı?** CLI ve connector Apache-2.0 lisanslıdır
([github.com/tunr-dev/tunr](https://github.com/tunr-dev/tunr)). Relay'in kaynak
kodu açıktır (PolyForm Shield) ve kendi sunucunuzda çalıştırılabilir.
