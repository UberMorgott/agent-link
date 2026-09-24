import { describe, expect, it } from 'vitest'
import { renderMarkdown } from './markdown'

function dom(src: string): HTMLElement {
  const div = document.createElement('div')
  div.innerHTML = renderMarkdown(src)
  return div
}

describe('renderMarkdown', () => {
  it('renders headings, lists, code and links', () => {
    const d = dom('## Новое\n\n- the `tray` icon\n- see [notes](https://example.com/n)\n\n```\ngo test\n```\n')
    expect(d.querySelector('h2')!.textContent).toBe('Новое')
    expect([...d.querySelectorAll('li')].map((li) => li.textContent)).toEqual(['the tray icon', 'see notes'])
    expect(d.querySelector('li code')!.textContent).toBe('tray')
    expect(d.querySelector('pre code')!.textContent).toBe('go test\n')
    const a = d.querySelector('a')!
    expect(a.getAttribute('href')).toBe('https://example.com/n')
    expect(a.getAttribute('target')).toBe('_blank')
    expect(a.getAttribute('rel')).toBe('noopener noreferrer')
  })

  it('links bare URLs, like GitHub\'s compare link', () => {
    const a = dom('**Full Changelog**: https://github.com/o/r/compare/v1...v2').querySelector('a')!
    expect(a.getAttribute('href')).toBe('https://github.com/o/r/compare/v1...v2')
    expect(a.getAttribute('rel')).toBe('noopener noreferrer')
  })

  it('escapes raw HTML instead of rendering it', () => {
    const src = '<script>alert(1)</script>\n\nhi <img src=x onerror=alert(1)> <b style="color:red">b</b>'
    const html = renderMarkdown(src)
    const d = dom(src)
    expect(d.querySelector('script, img, b, [style], [onerror]')).toBeNull()
    expect(html).toContain('&lt;script&gt;')
    expect(d.textContent).toContain('<img src=x onerror=alert(1)>')
  })

  it('drops dangerous link schemes and images', () => {
    const d = dom('[x](javascript:alert(1)) [y](data:text/html;base64,PHNjcmlwdD4=) [z](vbscript:msgbox)')
    expect(d.querySelector('a')).toBeNull()
    // An image stays a plain link: nothing loads from a remote host.
    const img = dom('![i](https://example.com/i.png)')
    expect(img.querySelector('img')).toBeNull()
    expect(img.querySelector('a')!.getAttribute('href')).toBe('https://example.com/i.png')
  })
})
