/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 'true' untuk memakai fixture; hanya berlaku saat DEV. */
  readonly VITE_USE_FIXTURES?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
