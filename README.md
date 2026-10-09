# shadowblade.space

Strona obsługuje HTTP/3 przez QUIC v2 i v1, z TLS 1.3. Nagłówek
`Alt-Svc` reklamuje HTTP/3 na UDP/443. HTTP/2 i HTTP/1.1 są dostępne
dla klientów, którzy nie mogą korzystać z QUIC. Biblioteka quic-go: v0.63.0.

## Licznik wejść

Plik `/root/site/poeple.log` zawiera tylko adres IP i czas UTC (RFC3339),
oddzielone spacją. Jest dopisywany po udanych żądaniach `GET /`, również
z parametrami zapytania i przy odpowiedzi 304. Wejścia na `/index.html`
są przekierowywane na `/` i liczone po przekierowaniu. Zasoby, HEAD,
błędy, odrzucone żądania i subdomeny nie są zapisywane.

To liczba wejść obsłużonych przez serwer, nie unikalnych osób. Odświeżenia
i boty pobierające stronę główną również są liczone.

Liczba wejść:

```sh
wc -l < /root/site/poeple.log
```

Log ma uprawnienia 0600 i właściciela 10001:10001 (użytkownik kontenera).
Jest montowany jako osobny plik poza katalogiem publicznym i zachowuje
zawartość po odtworzeniu kontenera. IP pochodzi z połączenia; nagłówki
`X-Forwarded-For` i `X-Real-IP` nie mogą go nadpisać. Docker wymaga,
aby plik istniał przed uruchomieniem usługi.

## Ograniczanie obciążenia

Limity dotyczą wyłącznie strony statycznej, łącznie dla wszystkich wersji HTTP:

- Strona główna: 1 żądanie/s na IP, zapas 5 żądań.
- Wszystkie ścieżki: 5 żądań/s na IP, zapas 30 żądań.
- Łącznie: 200 żądań/s, zapas 400 żądań.
- Przekroczenie limitu: HTTP 429 z `Retry-After: 1`.
- IPv6: wspólny limit dla /64; w logu pozostaje pełny adres.
- Rejestr limitów: najwyżej 10 000 wpisów, usuwanie nieaktywnych po 5 minutach.

HTML i zasoby są wczytywane do pamięci i kompresowane gzip raz przy starcie.
Zasoby mają cache na 24 godziny; HTML wymaga ponownego sprawdzenia ETag,
aby aktualizacje strony i licznik działały przy kolejnych żądaniach.
Lista publicznych plików obejmuje wyłącznie HTML i katalog assets.

QUIC dopuszcza 64 równoległe strumienie żądań na połączenie. Przy napływie
nowych połączeń przekraczającym 20/s (zapas 40) włącza weryfikację adresu
przez Retry. Limity aplikacyjne nie chronią przed zapełnieniem łącza;
ochrona przed takim atakiem wymaga filtrowania u dostawcy sieci.

## Budowanie i testy

Wymagane Go 1.26 lub nowsze. Docker buduje przy użyciu Go 1.26.8.

```sh
go test -race ./...
docker compose build shadowblade
docker compose up -d --no-deps shadowblade
curl --http3-only -I https://shadowblade.space/
```
