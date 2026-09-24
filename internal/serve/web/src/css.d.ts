// Side-effect CSS imports (e.g. "@xyflow/react/dist/style.css") have no
// type declarations of their own; esbuild bundles them, tsc just needs to
// know the specifier is importable.
declare module "*.css";
