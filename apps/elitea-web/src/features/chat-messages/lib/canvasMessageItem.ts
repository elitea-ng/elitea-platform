/**
 * The one place that names the stored `item_type` of a canvas item. The
 * transcript reads canvas items with it (`ui/chat-box/AnswerMessageItems`),
 * and the regenerate patch keeps them with it (#1098).
 */
const CANVAS_MESSAGE_ITEM_TYPE = 'canvas_message';

/** Whether one stored `message_items` entry is a canvas item. */
export function isCanvasMessageItem(item: unknown): boolean {
  return typeof item === 'object' && item !== null && (item as Record<string, unknown>)['item_type'] === CANVAS_MESSAGE_ITEM_TYPE;
}
