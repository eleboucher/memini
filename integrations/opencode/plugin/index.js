// opencode 1.x reads `server`, 2.x reads `setup`.
import v1 from "./memini.js";
import { setup } from "./memini-v2.js";

export default { ...v1, setup };
