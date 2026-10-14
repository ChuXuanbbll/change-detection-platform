import type { ImageInfo } from './api';
import type { TimelinePair } from './timeline';

/** Display order is top to bottom; equal dates have a stable source order. */
export function newestImagesFirst(a: ImageInfo, b: ImageInfo): number {
  return Date.parse(b.capture_date || '') - Date.parse(a.capture_date || '') || a.id.localeCompare(b.id);
}

export function newestResultsFirst(a?: TimelinePair, b?: TimelinePair): number {
  if (!a || !b) return a ? -1 : b ? 1 : 0;
  return newestImagesFirst(a.after, b.after) || newestImagesFirst(a.before, b.before) || a.key.localeCompare(b.key);
}

export function colorText(color: string): string {
  const channels = color.match(/^#([\da-f]{2})([\da-f]{2})([\da-f]{2})$/i);
  if (!channels) return '#fff';
  const [r, g, b] = channels.slice(1).map(value => parseInt(value, 16));
  return r * 0.299 + g * 0.587 + b * 0.114 > 145 ? '#0f172a' : '#fff';
}
