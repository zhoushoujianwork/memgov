// Presentation only: retain exact timestamps without changing server data.
const relative = new Intl.RelativeTimeFormat("zh-CN", { numeric: "always" });
const absolute = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric",
  month: "numeric",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
  timeZoneName: "short",
});
export function relativeTime(value, now = Date.now()) {
  if (!value) return "未记录";
  const stamp = Date.parse(value);
  if (!Number.isFinite(stamp)) return "时间未知";
  const seconds = (stamp - now) / 1000,
    distance = Math.abs(seconds);
  if (distance < 60) return seconds > 0 ? "即将" : "刚刚";
  const [unit, size] =
    distance < 3600
      ? ["minute", 60]
      : distance < 86400
        ? ["hour", 3600]
        : distance < 2592000
          ? ["day", 86400]
          : distance < 31536000
            ? ["month", 2592000]
            : ["year", 31536000];
  return relative.format(
    Math.sign(seconds) * Math.floor(distance / size),
    unit,
  );
}
export function updateTimeElement(
  element,
  value,
  { prefix = "", suffix = "", empty = "未记录" } = {},
) {
  const stamp = value ? Date.parse(value) : NaN;
  if (!Number.isFinite(stamp)) {
    element.textContent = prefix + (value ? "时间未知" : empty) + suffix;
    element.removeAttribute("datetime");
    element.removeAttribute("title");
    element.removeAttribute("aria-label");
    delete element.dataset.timestamp;
    delete element.dataset.prefix;
    delete element.dataset.suffix;
    return element;
  }
  const iso = new Date(stamp).toISOString();
  if (element.dataset.timestamp !== iso) {
    element.dateTime = iso;
    element.dataset.timestamp = iso;
    element.title = absolute.format(stamp);
  }
  if (element.dataset.prefix !== prefix) element.dataset.prefix = prefix;
  if (element.dataset.suffix !== suffix) element.dataset.suffix = suffix;
  const text = prefix + relativeTime(iso) + suffix;
  if (element.textContent !== text) element.textContent = text;
  const label = `${text}（${element.title}）`;
  if (element.getAttribute("aria-label") !== label)
    element.setAttribute("aria-label", label);
  return element;
}
export function timeElement(value, options) {
  return updateTimeElement(document.createElement("time"), value, options);
}
export function refreshRelativeTimes() {
  for (const element of document.querySelectorAll("time[data-timestamp]")) {
    const { timestamp, prefix, suffix } = element.dataset;
    updateTimeElement(element, timestamp, { prefix, suffix });
  }
}
