// Wrangler bundles .wasm imports as precompiled WebAssembly.Module values.
declare module "*.wasm" {
  const module: WebAssembly.Module;
  export default module;
}
