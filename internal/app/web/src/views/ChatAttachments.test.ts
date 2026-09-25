import { beforeEach, describe, expect, it } from 'vitest'
import { bodyText, clipName, fileURL, pastedFiles } from '@/lib/attachments'
import { fakeApi, HttpError, mountApp, settle } from '@/test/harness'
import type { ChatMessage } from '@/types'

const P = 'PROJ'
const prefix = 'projects/' + P + '/'
const sha = (c: string) => c.repeat(64)
const iso = (ms: number) => new Date(ms).toISOString()

let items: ChatMessage[]
const sent: Record<string, unknown>[] = []
const uploads: string[] = []

function serve() {
  return fakeApi((method, path, body) => {
    if (path === 'projects') return [{ id: P, legacy: false, name: 'Сайт', alias: '', display: 'Сайт', dir: 'W:/work', state: 'ready', problem: '', online: 1, total: 1, can_rename: true, has_invite: true, busy: false, members: [{ name: 'local', self: true, online: true }, { name: 'bob', online: true }] }]
    path = path.slice(prefix.length)
    if (method === 'POST' && path.startsWith('attachments?name=')) {
      const name = decodeURIComponent(path.slice('attachments?name='.length))
      uploads.push(name)
      if (name.endsWith('.exe')) throw new HttpError(400, 'refused', 'attachment_type')
      return { id: sha(String(uploads.length)), name, mime: name.endsWith('.png') ? 'image/png' : 'text/plain; charset=utf-8', size: 2048 }
    }
    if (method === 'POST' && path === 'send') {
      sent.push(body as Record<string, unknown>)
      return { id: 's1', chat_id: 'c1' }
    }
    const info = { id: 'c1', participants: ['bob', 'local'], closed: false, archived: false, last_seq: items.length, members: [{ name: 'bob', connected: true, compatible: true, queued: 0 }, { name: 'local', self: true, connected: true, compatible: true, queued: 0 }] }
    if (path === 'chats') return [info]
    if (path === 'chats/c1') return info
    if (path.startsWith('chats/c1/messages')) return items
    throw new Error('unexpected call ' + method + ' ' + path)
  })
}

const $ = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)
const $$ = <T extends Element = HTMLElement>(sel: string) => Array.from(document.querySelectorAll<T>(sel))

function transfer(files: File[]) {
  return { items: files.map((f) => ({ kind: 'file', getAsFile: () => f })), files }
}

function paste(files: File[], text = '') {
  const ev = new Event('paste', { bubbles: true, cancelable: true })
  Object.defineProperty(ev, 'clipboardData', { value: { ...transfer(files), getData: () => text } })
  $('#send textarea')!.dispatchEvent(ev)
  return ev
}

function drop(files: File[]) {
  const ev = new Event('drop', { bubbles: true, cancelable: true })
  Object.defineProperty(ev, 'dataTransfer', { value: transfer(files) })
  $('#send')!.dispatchEvent(ev)
}

const png = (name = 'image.png') => new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], name, { type: 'image/png' })

beforeEach(() => {
  items = []
  sent.length = 0
  uploads.length = 0
})

describe('attachment helpers', () => {
  it('strips fallback lines, names clipboard images and builds file URLs', () => {
    const atts = [{ id: sha('a'), name: 'a.png', mime: 'image/png', size: 1 }, { id: sha('b'), name: 'b.txt', mime: 'text/plain', size: 1 }]
    expect(bodyText('hi\n[attachment: a.png]\n[attachment: b.txt]', atts)).toBe('hi')
    expect(bodyText('[attachment: a.png]', atts.slice(0, 1))).toBe('')
    expect(bodyText('keep [attachment: a.png] inline', atts.slice(0, 1))).toBe('keep [attachment: a.png] inline')
    expect(clipName(png(), new Date(2026, 8, 25, 10, 5, 7))).toBe('clipboard-20260925-100507.png')
    expect(clipName(png('shot.png'))).toBe('shot.png')
    expect(fileURL('P 1', { id: sha('a'), name: 'a b.png' }, true)).toBe('/ui/files/P%201/' + sha('a') + '?name=a%20b.png&download=1')
    expect(pastedFiles(null)).toEqual([])
  })
})

describe('composer attachments', () => {
  it('pastes an image, shows its chip, removes one, drops a file and sends both', async () => {
    serve()
    await mountApp('/p/' + P + '/c/c1')
    await settle()
    // A text paste stays text.
    expect(paste([], 'plain').defaultPrevented).toBe(false)
    expect($('#composer_files')).toBeNull()

    expect(paste([png()]).defaultPrevented).toBe(true)
    await settle()
    expect(uploads).toEqual([expect.stringMatching(/^clipboard-\d{8}-\d{6}\.png$/)])
    const thumb = $<HTMLImageElement>('#composer_files img.composer-thumb')!
    expect(thumb.getAttribute('src')).toBe('/ui/files/PROJ/' + sha('1') + '?name=' + encodeURIComponent(uploads[0]!))

    drop([new File(['x'], 'notes.md', { type: 'text/markdown' }), png('second.png')])
    await settle()
    expect($$('#composer_files li').map((li) => li.dataset.file)).toEqual([uploads[0], 'notes.md', 'second.png'])
    $$<HTMLButtonElement>('#composer_files .composer-file-remove')[2]!.click()
    await settle()
    expect($$('#composer_files li')).toHaveLength(2)

    $<HTMLFormElement>('#send')!.requestSubmit()
    await settle()
    expect(sent).toHaveLength(1)
    expect(sent[0]!.attachments).toEqual([{ id: sha('1'), name: uploads[0] }, { id: sha('2'), name: 'notes.md' }])
    expect(sent[0]!.body).toBe('')
    expect($('#composer_files')).toBeNull()
  })

  it('refuses a file over the limit before uploading and a type the app refuses', async () => {
    serve()
    await mountApp('/p/' + P + '/c/c1')
    await settle()
    const big = new File(['x'], 'big.png', { type: 'image/png' })
    Object.defineProperty(big, 'size', { value: 11 << 20 })
    const input = $<HTMLInputElement>('#attach_input')!
    Object.defineProperty(input, 'files', { value: [big], configurable: true })
    input.dispatchEvent(new Event('change'))
    await settle()
    expect(uploads).toEqual([])
    expect($('#inbox_result')!.textContent!.trim()).toBe('inbox.attach.too_big') // the page's sentence names the file

    drop([new File(['MZ'], 'tool.exe')])
    await settle()
    expect(uploads).toEqual(['tool.exe'])
    expect($('#composer_files')).toBeNull()
    expect($('#inbox_result')!.textContent).toContain('tool.exe')
  })
})

describe('message attachments', () => {
  it('shows image thumbnails, file chips with download, failed files, and hides fallback lines', async () => {
    items = [{
      id: 'm1', seq: 1, from: 'bob', direction: 'in', created_at: iso(1700000000000),
      body: 'смотри\n[attachment: shot.png]\n[attachment: log.txt]\n[attachment: lost.pdf]',
      attachments: [
        { id: sha('a'), name: 'shot.png', mime: 'image/png', size: 4000 },
        { id: sha('b'), name: 'log.txt', mime: 'text/plain; charset=utf-8', size: 3 << 20 },
        { id: sha('c'), name: 'lost.pdf', mime: '', size: 0, failed: true },
      ],
    }]
    serve()
    await mountApp('/p/' + P + '/c/c1')
    await settle()
    const bubble = $('[data-message-id="m1"]')!
    expect(bubble.querySelector('.msg-body')!.textContent).toBe('смотри')
    const img = bubble.querySelector<HTMLImageElement>('.msg-image img')!
    expect(img.getAttribute('src')).toBe('/ui/files/PROJ/' + sha('a') + '?name=shot.png')
    const file = bubble.querySelector<HTMLAnchorElement>('a.msg-file')!
    expect(file.getAttribute('href')).toBe('/ui/files/PROJ/' + sha('b') + '?name=log.txt&download=1')
    expect(file.getAttribute('download')).toBe('log.txt')
    expect(bubble.querySelector('.msg-file.failed')!.textContent).toContain('lost.pdf')
  })
})
