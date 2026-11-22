# NestNAS landing page

The marketing site for NestNAS, deployed at <https://nestnas.vercel.app>.

Plain HTML, CSS and a little JavaScript. There is no build step and no framework.

## Preview locally

```bash
cd site
python3 -m http.server 8000      # then open http://localhost:8000
```

- `?nosplash` skips the boot animation; `?static` also settles every animation (used for screenshots).
- Honours `prefers-reduced-motion`.

## Deploy

The folder deploys as-is to Vercel (`vercel.json` sets the security headers and caching):

```bash
cd site
npx vercel deploy --prod
```

## Notes

- **No third-party requests.** The fonts are self-hosted in `fonts/`, so visitors' browsers only talk to this site.
- **Fonts** are licensed under the SIL Open Font License 1.1: [Fraunces](https://github.com/undercasetype/Fraunces), [Instrument Serif](https://github.com/Instrument/instrument-serif), [Geist and Geist Mono](https://github.com/vercel/geist-font).
- `og.png` is the social-preview image (1200x630).
- The numbers in the "Receipts" section (lines of Go, checks, binary size) come from the repository; update them when they change meaningfully.
