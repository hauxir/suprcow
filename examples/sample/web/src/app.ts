// 👇 Edit this file while the preview is open and watch it hot-reload in place —
// no rebuild, no restart, no waiting page. That's the suprcow feedback loop.
const HEADLINE = '🐮 suprcow sample'

export async function render(el: HTMLElement) {
  el.innerHTML = `<h1>${HEADLINE}</h1><p>loading…</p>`
  try {
    const res = await fetch('/api/hello')
    const data = await res.json()
    el.innerHTML = `
      <h1>${HEADLINE}</h1>
      <p>${data.message}</p>
      <small>api time: ${data.time}</small>
    `
  } catch (err) {
    el.innerHTML = `<h1>${HEADLINE}</h1><p>API unreachable: ${String(err)}</p>`
  }
}
