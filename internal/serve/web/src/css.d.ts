// Side-effect CSS imports ("./flow.css") have no type declarations of their
// own; esbuild bundles them, tsc just needs to know the specifier is
// importable.
declare module "*.css";
