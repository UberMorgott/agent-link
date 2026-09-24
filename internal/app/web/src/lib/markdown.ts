import MarkdownIt from 'markdown-it'

// Release notes are Markdown written on GitHub. They render with raw HTML
// off (markdown-it escapes it as text) and markdown-it's link validation,
// which drops javascript:, vbscript:, file: and data: URLs. Images are off:
// the page's CSP would block remote ones anyway. Links open in a new tab
// without handing it this page.
const md = new MarkdownIt({ html: false, linkify: true }).disable('image')

const renderLinkOpen = md.renderer.rules.link_open
  ?? ((tokens, idx, options, _env, self) => self.renderToken(tokens, idx, options))
md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
  tokens[idx].attrSet('target', '_blank')
  tokens[idx].attrSet('rel', 'noopener noreferrer')
  return renderLinkOpen(tokens, idx, options, env, self)
}

export function renderMarkdown(src: string): string {
  return md.render(src)
}
