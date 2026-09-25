import { fmt } from '@/lib/runtime'
import type { Attachment } from '@/types'

// The node's limits (internal/node/attach.go); the node checks them again,
// with the type of each file.
export const MAX_FILES = 10
export const MAX_SIZE = 10 << 20
export const MAX_TOTAL = 50 << 20

// fileURL is where the app serves a message's file: images show inline,
// download=1 saves any file under its name.
export function fileURL(project: string, a: Pick<Attachment, 'id' | 'name'>, download = false): string {
  return '/ui/files/' + encodeURIComponent(project) + '/' + encodeURIComponent(a.id) +
    '?name=' + encodeURIComponent(a.name) + (download ? '&download=1' : '')
}

export function isImage(a: Pick<Attachment, 'mime'>): boolean {
  return a.mime.startsWith('image/')
}

// fallbackLine is the body line that stands for a file on peers without
// attachments: «[attachment: name]».
export function fallbackLine(a: Pick<Attachment, 'name'>): string {
  return '[attachment: ' + a.name + ']'
}

// bodyText is a message's text without the trailing fallback lines of its
// files, which the page shows as files.
export function bodyText(body: string | undefined, atts: Attachment[] | undefined): string {
  const text = body || ''
  if (!atts?.length) return text
  const lines = text.split('\n')
  for (let i = atts.length - 1; i >= 0 && lines.length; i--) {
    if (lines[lines.length - 1] !== fallbackLine(atts[i]!)) break
    lines.pop()
  }
  return lines.join('\n').replace(/\n+$/, '')
}

// sizeText is a byte count for people, in bytes, KB or MB.
export function sizeText(n: number): string {
  if (n < 1024) return fmt("inbox.attach.bytes", { n })
  if (n < 1 << 20) return fmt("inbox.attach.kb", { n: Math.round(n / 1024) })
  return fmt("inbox.attach.mb", { n: (n / (1 << 20)).toFixed(1) })
}

// pastedFiles is the files a paste carries (a screenshot from the clipboard);
// empty for text.
export function pastedFiles(data: DataTransfer | null): File[] {
  if (!data) return []
  const out: File[] = []
  for (const item of Array.from(data.items || [])) {
    if (item.kind !== 'file') continue
    const f = item.getAsFile()
    if (f) out.push(f)
  }
  return out.length ? out : Array.from(data.files || [])
}

// clipName names a pasted image that has no name of its own.
export function clipName(f: File, now = new Date()): string {
  if (f.name && !/^image\.\w+$/.test(f.name)) return f.name // browsers call every pasted image image.png
  const ext = (f.type.split('/')[1] || 'png').replace('jpeg', 'jpg')
  const pad = (n: number) => String(n).padStart(2, '0')
  return 'clipboard-' + now.getFullYear() + pad(now.getMonth() + 1) + pad(now.getDate()) + '-' +
    pad(now.getHours()) + pad(now.getMinutes()) + pad(now.getSeconds()) + '.' + ext
}
