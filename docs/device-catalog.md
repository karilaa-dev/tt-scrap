# Device catalog

Verified on **2026-10-13**. The selection window is **2022-10-13 through 2026-10-13**, inclusive. The catalog contains **75 phone models: 21 Google Pixel and 54 Samsung Galaxy**, with **184 model/Android combinations**. It covers Pixel A, base, Pro, XL and folding phones, and Galaxy A, S, FE, Edge, Flip, Fold and TriFold phones. It selects one documented hardware variant of each model; carrier, regional duplicate SKUs, tablets and region-only Fold Special Edition are outside this catalog.

This is a selected catalog spanning budget through flagship phones, not an inventory of every Samsung release. Galaxy M, Galaxy F and rugged XCover families are not included. They contain eligible recent phones, such as the officially released [Galaxy M36](https://news.samsung.com/in/samsung-launches-galaxy-m36-5g-in-india-introduces-advanced-ai-innovations-in-mid-segment-smartphones), [Galaxy F56](https://www.samsung.com/in/smartphones/galaxy-f/galaxy-f56-5g-green-128gb-sm-e566bzgains/) and [Galaxy XCover7](https://news.samsung.com/global/tag/galaxy-xcover7); their omission is a scope choice, not a claim that they are unavailable or unsupported.

`tiktokapp.PickProfile()` draws a phone uniformly, then draws uniformly from that phone's **latest three Android major versions with published firmware**, or all published versions if fewer than three exist. It does not substitute an update promise for released firmware. For example, the selected Galaxy A37 firmware history still ends at Android 16, while A57 has Android 17; Galaxy S24 ends at 16, while S24 FE has 17. Pixel 11 has only Android 17. Older A04e has 14, 13 and 12. The model and selected OS/build remain together in the persisted identity. A newly created or replaced identity gets a fresh selection.

`tiktokapp.Catalog()` exposes a copy of the checked-in catalog. Profile selection returns a fresh value, so changing one identity does not mutate another identity or the catalog.

## Evidence and interpretation

The [Google Pixel specifications](https://support.google.com/pixelphone/answer/7158570?hl=en) establish model names and native displays. Google's [factory image archive](https://developers.google.cn/android/images?hl=en) establishes released Android versions and exact Android build IDs. Each row below links to the model's factory image section. The selected image is the latest ordinary global release in each of the latest three major versions, excluding carrier-specific images and previews. A published month is retained as a month; a day is not invented.

Samsung's official firmware histories establish each model's Android versions, firmware identifiers and release dates. Each row links directly to its English history. Most international B/G variants use EUX; budget F variants use INS or XFA where that history is complete. A07 uses SM-A075M's published history, A17 uses SM-A175F's complete published history, and TriFold uses the XSP history. Firmware availability can differ by region; these are concrete shipped combinations for the selected model, not a claim that every region received them simultaneously. The [Google Play supported-device list](https://storage.googleapis.com/play_public/supported_devices.csv) supplies the model-code and device-codename associations.

Samsung's [2023 report](https://images.samsung.com/is/content/samsung/assets/global/ir/docs/2023_4Q_Interim_Report.pdf), [2025 report](https://images.samsung.com/is/content/samsung/assets/global/ir/docs/2025_4Q_Interim_Report.pdf) and [2026 first-quarter report](https://images.samsung.com/is/content/samsung/assets/global/ir/docs/2026_1Q_Interim_Report.pdf) list release months and displays for the historical A/S/Z models. Newer models use the [A37/A57 announcement](https://news.samsung.com/sg/samsung-unveils-galaxy-a57-5g-and-galaxy-a37-5g-packing-pro-level-features-at-awesome-price), [S26 FE announcement](https://news.samsung.com/global/samsung-galaxy-s26-fe-delivering-the-latest-flagship-experience-focused-on-what-matters-most), [Fold8/Flip8 announcement](https://news.samsung.com/global/samsung-galaxy-z-fold8-ultra-fold8-and-flip8foldables-perfected-for-every-way-of-living) and [TriFold announcement](https://news.samsung.com/global/introducing-galaxy-z-trifold-the-shape-of-whats-next-in-mobile-innovation). A04e's display comes from its [official product specifications](https://www.samsung.com/ph/smartphones/galaxy-a/galaxy-a04e-black-32gb-sm-a042fzkdphl/).

The availability column records a documented on-sale date or month, rather than a claim about the first sale in every country. Exact dates use the launch references below; report-only entries retain month precision. For example, A15's listed January 2024 is its report date, while some markets received it in December 2023. Both fall within this catalog's window.

Android/API mappings are explicit: **12→31, 13→33, 14→34, 15→35, 16→36, 17→37**. Android 12L/API 32 is a separate release and is not substituted for Android 12. Sources: [Android platform build references](https://source.android.com/docs/setup/reference/build-numbers) and [Android 17 SDK setup](https://developer.android.com/about/versions/17/setup-sdk).

## Profile fields

Native display resolutions are written in portrait orientation as `width*height`. Folding models use the unfolded main display, including TriFold. Physical pixels per inch are published Pixel values or rounded calculations from Samsung's native resolution and nominal diagonal. The transmitted `dpi` is a chosen logical display density, rounded to a nearby multiple of 20. It is a plausible Android display configuration, **not a verified factory density setting**; physical PPI and Android logical density are different quantities.

Pixel `build` values are Google's published Android build IDs. Samsung `build` values are Samsung's exact published firmware version strings, identified as `samsung-firmware-version` in the catalog. Samsung's firmware history does not establish the corresponding `ro.build.id`, full `ro.build.fingerprint`, signing certificate or `ro.build.tags`; those values are not fabricated. The protocol currently transmits the selected `build` string in its user agent and has no full-fingerprint or build-tags field.

The current app protocol sends `arm64-v8a` as its configured application ABI. The catalog's model codes and codenames are verified; the manufacturer sources do not establish every model's installed userspace ABI, especially lower-cost models with 64-bit-capable processors. Consequently this catalog does not claim to be a capture of stock Android properties or attestations from 75 physical phones. Physical-device `getprop` captures would be needed to replace the explicitly configured density/ABI and establish full stock fingerprints. Locale remains the client's existing English/US/UTC configuration.

## Google Pixel

Each firmware link contains the builds and dates recorded in `internal/tiktokapp/devices.json`. Android versions are newest first.

| Phone | Documented availability | Native display | Available Android choices | Firmware evidence |
| --- | --- | --- | --- | --- |
| Pixel 7 | 2022-10-13 | 1080 × 2400 | 17, 16, 15 | [panther](https://developers.google.cn/android/images?hl=en#panther) |
| Pixel 7 Pro | 2022-10-13 | 1440 × 3120 | 17, 16, 15 | [cheetah](https://developers.google.cn/android/images?hl=en#cheetah) |
| Pixel 7a | 2023-05-10 | 1080 × 2400 | 17, 16, 15 | [lynx](https://developers.google.cn/android/images?hl=en#lynx) |
| Pixel Fold | 2023-06-27 | 1840 × 2208 | 17, 16, 15 | [felix](https://developers.google.cn/android/images?hl=en#felix) |
| Pixel 8 | 2023-10-12 | 1080 × 2400 | 17, 16, 15 | [shiba](https://developers.google.cn/android/images?hl=en#shiba) |
| Pixel 8 Pro | 2023-10-12 | 1344 × 2992 | 17, 16, 15 | [husky](https://developers.google.cn/android/images?hl=en#husky) |
| Pixel 8a | 2024-05-14 | 1080 × 2400 | 17, 16, 15 | [akita](https://developers.google.cn/android/images?hl=en#akita) |
| Pixel 9 | 2024-08-22 | 1080 × 2424 | 17, 16, 15 | [tokay](https://developers.google.cn/android/images?hl=en#tokay) |
| Pixel 9 Pro | 2024-09-04 | 1280 × 2856 | 17, 16, 15 | [caiman](https://developers.google.cn/android/images?hl=en#caiman) |
| Pixel 9 Pro XL | 2024-08-22 | 1344 × 2992 | 17, 16, 15 | [komodo](https://developers.google.cn/android/images?hl=en#komodo) |
| Pixel 9 Pro Fold | 2024-09-04 | 2076 × 2152 | 17, 16, 15 | [comet](https://developers.google.cn/android/images?hl=en#comet) |
| Pixel 9a | 2025-04-10 | 1080 × 2424 | 17, 16, 15 | [tegu](https://developers.google.cn/android/images?hl=en#tegu) |
| Pixel 10 | 2025-08-28 | 1080 × 2424 | 17, 16 | [frankel](https://developers.google.cn/android/images?hl=en#frankel) |
| Pixel 10 Pro | 2025-08-28 | 1280 × 2856 | 17, 16 | [blazer](https://developers.google.cn/android/images?hl=en#blazer) |
| Pixel 10 Pro XL | 2025-08-28 | 1344 × 2992 | 17, 16 | [mustang](https://developers.google.cn/android/images?hl=en#mustang) |
| Pixel 10 Pro Fold | 2025-10-09 | 2076 × 2152 | 17, 16 | [rango](https://developers.google.cn/android/images?hl=en#rango) |
| Pixel 10a | 2026-03-05 | 1080 × 2424 | 17, 16 | [stallion](https://developers.google.cn/android/images?hl=en#stallion) |
| Pixel 11 | 2026-08-20 | 1080 × 2424 | 17 | [cubs](https://developers.google.cn/android/images?hl=en#cubs) |
| Pixel 11 Pro | 2026-08-20 | 1280 × 2856 | 17 | [grizzly](https://developers.google.cn/android/images?hl=en#grizzly) |
| Pixel 11 Pro XL | 2026-08-20 | 1344 × 2992 | 17 | [kodiak](https://developers.google.cn/android/images?hl=en#kodiak) |
| Pixel 11 Pro Fold | 2026-08-20 | 2076 × 2152 | 17 | [yogi](https://developers.google.cn/android/images?hl=en#yogi) |

## Samsung Galaxy

The model code identifies the selected hardware variant. The linked history and each `releasedAt` value in source are the firmware evidence, rather than the model's promised support period.

| Phone | Model code | Documented availability | Available Android choices | Firmware evidence |
| --- | --- | --- | --- | --- |
| Galaxy A04e | SM-A042F | 2022-11 | 14, 13, 12 | [INS](https://doc.samsungmobile.com/SM-A042F/025512221202/eng.html) |
| Galaxy A14 | SM-A145F | 2023-03 | 15, 14, 13 | [INS](https://doc.samsungmobile.com/SM-A145F/026209230304/eng.html) |
| Galaxy A14 5G | SM-A146B | 2023-01 | 15, 14, 13 | [INS](https://doc.samsungmobile.com/SM-A146B/025563221227/eng.html) |
| Galaxy A24 | SM-A245F | 2023-05 | 16, 15, 14 | [XFA](https://doc.samsungmobile.com/SM-A245F/026883230628/eng.html) |
| Galaxy A34 5G | SM-A346B | 2023-03 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A346B/026525230403/eng.html) |
| Galaxy A54 5G | SM-A546B | 2023-03 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A546B/026481230324/eng.html) |
| Galaxy A05 | SM-A055F | 2023-10 | 15, 14, 13 | [INS](https://doc.samsungmobile.com/SM-A055F/028080231020/eng.html) |
| Galaxy A05s | SM-A057G | 2023-10 | 15, 14, 13 | [EUX](https://doc.samsungmobile.com/SM-A057G/028568240123/eng.html) |
| Galaxy A15 | SM-A155F | 2024-01 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A155F/028996240217/eng.html) |
| Galaxy A15 5G | SM-A156B | 2024-01 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A156B/028995240217/eng.html) |
| Galaxy A25 5G | SM-A256B | 2024-01 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A256B/028619240129/eng.html) |
| Galaxy A35 5G | SM-A356B | 2024-03 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A356B/029890240424/eng.html) |
| Galaxy A55 5G | SM-A556B | 2024-03 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A556B/029681240325/eng.html) |
| Galaxy A06 | SM-A065F | 2024-09 | 16, 15, 14 | [INS](https://doc.samsungmobile.com/SM-A065F/031048241024/eng.html) |
| Galaxy A16 | SM-A165F | 2024-11 | 16, 15, 14 | [XFA](https://doc.samsungmobile.com/SM-A165F/031475241121/eng.html) |
| Galaxy A16 5G | SM-A166B | 2024-10 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-A166B/031022241021/eng.html) |
| Galaxy A06 5G | SM-A066B | 2025-02 | 16, 15 | [INS](https://doc.samsungmobile.com/SM-A066B/032777250226/eng.html) |
| Galaxy A26 5G | SM-A266B | 2025-03 | 16, 15 | [EUX](https://doc.samsungmobile.com/SM-A266B/033044250414/eng.html) |
| Galaxy A36 5G | SM-A366B | 2025-03 | 16, 15 | [EUX](https://doc.samsungmobile.com/SM-A366B/033045250414/eng.html) |
| Galaxy A56 5G | SM-A566B | 2025-03 | 16, 15 | [EUX](https://doc.samsungmobile.com/SM-A566B/033284250424/eng.html) |
| Galaxy A07 | SM-A075M | 2025-08 | 16, 15 | [documented-history](https://doc.samsungmobile.com/SM-A075M/035618250930/eng.html) |
| Galaxy A17 | SM-A175F | 2025-09 | 16, 15 | [documented-history](https://doc.samsungmobile.com/SM-A175F/035884251023/eng.html) |
| Galaxy A17 5G | SM-A176B | 2025-08 | 16, 15 | [EUX](https://doc.samsungmobile.com/SM-A176B/035401250915/eng.html) |
| Galaxy A07 5G | SM-A076B | 2026-01 | 16 | [INS](https://doc.samsungmobile.com/SM-A076B/036774260202/eng.html) |
| Galaxy A37 5G | SM-A376B | 2026-04-10 | 16 | [EUX](https://doc.samsungmobile.com/SM-A376B/037946260421/eng.html) |
| Galaxy A57 5G | SM-A576B | 2026-04-10 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-A576B/037652260330/eng.html) |
| Galaxy S23 | SM-S911B | 2023-02-17 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S911B/025800230221/eng.html) |
| Galaxy S23+ | SM-S916B | 2023-02-17 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S916B/025799230221/eng.html) |
| Galaxy S23 Ultra | SM-S918B | 2023-02-17 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S918B/025798230221/eng.html) |
| Galaxy S23 FE | SM-S711B | 2023-10-05 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S711B/028494231223/eng.html) |
| Galaxy S24 | SM-S921B | 2024-01-31 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S921B/029280240224/eng.html) |
| Galaxy S24+ | SM-S926B | 2024-01-31 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S926B/029278240224/eng.html) |
| Galaxy S24 Ultra | SM-S928B | 2024-01-31 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-S928B/029279240224/eng.html) |
| Galaxy S24 FE | SM-S721B | 2024-10 | 17, 16, 15 | [EUX](https://doc.samsungmobile.com/SM-S721B/030823241004/eng.html) |
| Galaxy S25 | SM-S931B | 2025-02-07 | 17, 16, 15 | [EUX](https://doc.samsungmobile.com/SM-S931B/032402250224/eng.html) |
| Galaxy S25+ | SM-S936B | 2025-02-07 | 17, 16, 15 | [EUX](https://doc.samsungmobile.com/SM-S936B/032403250224/eng.html) |
| Galaxy S25 Ultra | SM-S938B | 2025-02-07 | 17, 16, 15 | [EUX](https://doc.samsungmobile.com/SM-S938B/032404250224/eng.html) |
| Galaxy S25 Edge | SM-S937B | 2025-05 | 17, 16, 15 | [EUX](https://doc.samsungmobile.com/SM-S937B/033965250612/eng.html) |
| Galaxy S25 FE | SM-S731B | 2025-09 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-S731B/035499250925/eng.html) |
| Galaxy S26 | SM-S942B | 2026-03-11 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-S942B/037104260310/eng.html) |
| Galaxy S26+ | SM-S947B | 2026-03-11 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-S947B/037105260310/eng.html) |
| Galaxy S26 Ultra | SM-S948B | 2026-03-11 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-S948B/037106260310/eng.html) |
| Galaxy S26 FE | SM-S741B | 2026-09-04 | 17 | [EUX](https://doc.samsungmobile.com/SM-S741B/039937260904/eng.html) |
| Galaxy Z Fold5 | SM-F946B | 2023-08-11 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-F946B/027070230817/eng.html) |
| Galaxy Z Flip5 | SM-F731B | 2023-08-11 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-F731B/027071230817/eng.html) |
| Galaxy Z Fold6 | SM-F956B | 2024-07-24 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-F956B/030575240902/eng.html) |
| Galaxy Z Flip6 | SM-F741B | 2024-07-24 | 16, 15, 14 | [EUX](https://doc.samsungmobile.com/SM-F741B/030574240902/eng.html) |
| Galaxy Z Fold7 | SM-F966B | 2025-07-25 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-F966B/034396250731/eng.html) |
| Galaxy Z Flip7 | SM-F766B | 2025-07-25 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-F766B/034395250731/eng.html) |
| Galaxy Z Flip7 FE | SM-F761B | 2025-07-25 | 17, 16 | [EUX](https://doc.samsungmobile.com/SM-F761B/034394250731/eng.html) |
| Galaxy Z Fold8 Ultra | SM-F976B | 2026-08-07 | 17 | [EUX](https://doc.samsungmobile.com/SM-F976B/039257260801/eng.html) |
| Galaxy Z Fold8 | SM-F971B | 2026-08-07 | 17 | [EUX](https://doc.samsungmobile.com/SM-F971B/039256260801/eng.html) |
| Galaxy Z Flip8 | SM-F776B | 2026-08-07 | 17 | [EUX](https://doc.samsungmobile.com/SM-F776B/039255260801/eng.html) |
| Galaxy Z TriFold | SM-F968B | 2025-12 | 17, 16 | [XSP](https://doc.samsungmobile.com/SM-F968B/036568260119/eng.html) |

## Exact availability references

Pixel: [7/7 Pro](https://support.google.com/pixelphone/thread/182637394/the-pixel-7-and-7-pro-are-here?hl=en), [7a](https://blog.google/products-and-platforms/devices/pixel/pixel-7a-io-2023/), [Fold](https://support.google.com/pixelphone/thread/215074384/introducing-the-pixel-fold?hl=en-gb), [8/8 Pro](https://blog.google/intl/en-ca/products/devices-services/meet-pixel-8-and-pixel-8-pro-our-newest-phones/), [8a](https://blog.google/products-and-platforms/devices/pixel/pixel-8a-launch/), [9/9 Pro/9 Pro XL](https://blog.google/products-and-platforms/devices/pixel/google-pixel-9-pro-xl/), [9 Pro Fold](https://blog.google/products-and-platforms/devices/pixel/google-pixel-9-pro-fold/), [9a](https://blog.google/products-and-platforms/devices/pixel/google-pixel-9a-available-now/), [10/10 Pro/10 Pro XL](https://blog.google/products-and-platforms/devices/pixel/pixelsnap-accessories-charger-ring-stand/), [10 Pro Fold](https://blog.google/products-and-platforms/devices/pixel/google-pixel-10-pro-fold/), [10a](https://blog.google/products-and-platforms/devices/pixel/google-pixel-10a/), [11 series](https://blog.google/products-and-platforms/devices/pixel/pixel-11-helpful-updates-features/). Google also maintains a [first-availability summary](https://support.google.com/pixelphone/answer/15738422?hl=en).

Samsung: [S23](https://news.samsung.com/global/samsung-announces-global-launch-of-the-galaxy-s23-series), [S23 FE](https://news.samsung.com/global/samsung-galaxy-s23-fe-galaxy-tab-s9-fe-and-galaxy-buds-fe-bring-standout-features-to-even-more-users), [S24](https://www.samsung.com/us/explore/mobile/buying-guide/introducing-samsung-galaxy-s24/), [S25](https://www.samsung.com/ae/news/local/samsung-launches-galaxy-s25-series-in-the-uae-pioneering-a-new-era-of-ai-powered-mobile-experiences/), [S26](https://insights.samsung.com/2026/02/25/samsung-unpacked-february-2026-full-replay-and-highlights/), [A37/A57 UK availability](https://news.samsung.com/uk/tag/galaxy-a57-5g), [Fold5/Flip5](https://news.samsung.com/global/samsung-galaxy-z-flip5-and-galaxy-z-fold5-delivering-flexibility-and-versatility-without-compromise), [Fold6/Flip6](https://news.samsung.com/global/samsung-galaxy-z-fold-6-and-z-flip-6-elevate-galaxy-ai-to-new-heights), [Fold7/Flip7](https://news.samsung.com/global/samsung-galaxy-z-fold7-raising-the-bar-for-smartphones), [Fold8/Flip8 availability](https://news.samsung.com/us/samsung-launches-galaxy-z-fold8-ultra-fold8-flip8-watch-ultra2-watch9/). S26 FE and TriFold use their announcements linked above.

## Refreshing the snapshot

Advance both snapshot bounds together. Remove phones whose documented original availability falls before the new window. Add a model only after a manufacturer release/specification source and its actual firmware history exist. For each model and region, select at most the three newest distinct Android major versions and retain the exact published build string and release date. An update schedule, support promise or beta does not establish a released version. Check native display dimensions and the selected model code, then run `go test ./internal/tiktokapp` and `go vet ./...`.

The snapshot is checked in; runtime identity creation does not scrape vendor websites or automatically predict future devices. Tests cover every selectable combination, API mapping, availability boundaries, model/Android coverage and independent returned profiles.
