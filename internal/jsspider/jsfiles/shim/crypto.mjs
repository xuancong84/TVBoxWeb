import { createRequire } from 'module';
const require = createRequire(import.meta.url);
const CJ = require('../libs/crypto-js.cjs');
globalThis.CryptoJS = CJ;
export default CJ;
