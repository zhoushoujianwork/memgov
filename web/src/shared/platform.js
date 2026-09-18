// Small browser defaults; desktop integrations stay outside page code.
export const platform = {
  async copy(text) {
    if (!navigator.clipboard)
      throw new Error("此浏览器无法自动复制，请选中命令复制。");
    await navigator.clipboard.writeText(text);
  },
};
