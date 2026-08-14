# Gerege Nexus App Store

Апп сторын бүтээгдэхүүн: **registry** (каталог нийтлэх, гарын үсэг зурах),
**publisher studio** (гуравдагч тал апп илгээх), **review** (хянаж нийтлэх).

Энэ бол [Gerege Nexus](https://github.com/gerege-systems/open-gerege-nexus)
платформын дээр баригдсан **distribution** — экосистемийн git стратегийн
[Түвшин 2](https://github.com/gerege-systems/open-gerege-nexus/blob/main/docs/ECOSYSTEM_GIT_STRATEGY.md).

## Энд юу байгаа, юу байхгүй вэ

Энд платформын код **нэг ч мөр байхгүй**. Нэвтрэлт, тенант, өгөгдлийн сан ба
түүний тусгаарлалт, аппын gate, цэс, HTTP сервер бүгд цөмийнх бөгөөд
`go.mod`-ын нэг мөрөөр tag-аар авагдана:

```
require github.com/gerege-systems/open-gerege-nexus/backend v1.1.0
```

Энэ repo нэмдэг зүйл нь гурван модуль ба тэднийг бүртгэх мөр:

```
main.go            платформыг асааж, гурван модулиа бүртгэнэ
modules/registry   каталог, гарын үсэг, нийтлэлийн сувгууд
modules/publisher  нийтлэгчийн профайл, апп бүртгэл, хувилбар илгээх
modules/review     хянан батлах дараалал
catalog/           энэ бүтээгдэхүүний апп багц + manifest
```

## Ажиллуулах

```bash
go build ./...
go test ./...
```

Локал орчинд цөмтэй зэрэг ажиллах бол `go.work` (commit хийхгүй):

```bash
go work init . ../open-gerege-nexus/backend
```

## Дүрмүүд

Цөмийн [CONTRIBUTING](https://github.com/gerege-systems/open-gerege-nexus/blob/main/CONTRIBUTING.md)-ийн
дүрмүүд энд мөрдөгдөнө. Хамгийн чухал хоёр нь:

1. **Upstream-first** — цөмд засвар хэрэгтэй бол цөм рүү PR илгээнэ. Энд
   хуулбарлаж засахыг хориглоно; тэр нь зуун repo-гийн drift-ийн эх үүсвэр.
2. **Бизнес логик зөвхөн модуль хэлбэрээр** — `main.go` дотор custom код
   бичихгүй.

## Хувилбар

Цөмийн шинэ tag гармагц Renovate `go.mod`-ын мөрийг өсгөх PR нээнэ. CI
ногоон бол merge; улаан бол цөмийн release-д асуудал байна гэсэн дохио тул
цөмийн багт issue очно.

## Лиценз

Apache 2.0 — `LICENSE`-ийг үзнэ үү.
