// cytoscape-cola ships no type declarations; it's a cytoscape layout plugin
// registered via cytoscape.use(). We only pass it to use(), so an opaque
// module type is sufficient.
declare module "cytoscape-cola" {
  import type { Ext } from "cytoscape";
  const cola: Ext;
  export default cola;
}
