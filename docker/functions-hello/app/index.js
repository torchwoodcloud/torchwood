// functions-hello：镜像源示例函数（main 风格）。
exports.main = async (data, ctx) => ({
  ok: true,
  echo: data,
  via: "byo-image",
  source: ctx.source,
});
