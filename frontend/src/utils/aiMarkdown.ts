import MarkdownIt from 'markdown-it';

const markdown = new MarkdownIt({ html: false, linkify: false, breaks: true });
markdown.disable('image');

const validateLink = markdown.validateLink.bind(markdown);
markdown.validateLink = (url: string): boolean =>
  validateLink(url) && (!/^[a-z][a-z\d+.-]*:/i.test(url) || /^(?:https?|mailto):/i.test(url));

const renderLink = markdown.renderer.rules.link_open;
markdown.renderer.rules.link_open = (tokens, index, options, environment, renderer) => {
  tokens[index].attrSet('rel', 'noopener noreferrer');
  if (/^https?:\/\//i.test(tokens[index].attrGet('href') || ''))
    tokens[index].attrSet('target', '_blank');
  return renderLink
    ? renderLink(tokens, index, options, environment, renderer)
    : renderer.renderToken(tokens, index, options);
};

/** The only HTML entry point for provider text; raw HTML and images are disabled. */
export function renderAiMarkdown(text: string): string {
  return markdown.render(text);
}
