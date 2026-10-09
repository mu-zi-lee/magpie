import { pathToFileURL } from "node:url"

// This process loads only the declared cleanup module: no normal plugin factory,
// sign-in, model discovery or automatic runtime installation is run on removal.
try {
  const params = JSON.parse(await Bun.stdin.text())
  const module = await import(pathToFileURL(params.entry).href)
  if (typeof module.default !== "function") throw new Error("uninstall module must export a default function")
  await module.default({ directory: params.directory, worktree: params.directory, reason: "uninstall" }, params.options ?? {})
  process.exit(0)
} catch {
  // A cleanup module's exception can contain credentials. Report failure without
  // copying arbitrary module output into magpie's logs or API response.
  process.exit(1)
}
