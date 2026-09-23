// Installed by quality-gate. Edit freely -- it is yours once it lands here.
//
// Needs: npm i -D eslint @eslint/js typescript-eslint eslint-plugin-vue globals
import js from '@eslint/js'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'
import ts from 'typescript-eslint'

export default ts.config(
  { ignores: ['dist/**', 'coverage/**', '.cache/**'] },
  js.configs.recommended,
  ...ts.configs.recommended,
  ...pluginVue.configs['flat/recommended'],
  {
    // <script lang="ts"> inside .vue needs the TS parser handed to vue-eslint-parser.
    // typescript-eslint turns no-undef off only for .ts files, so .vue code runs it
    // too: declare the browser globals rather than switch the rule off, so a name
    // that really is undefined still fails in a plain <script> as well.
    files: ['*.vue', '**/*.vue'],
    languageOptions: {
      globals: globals.browser,
      parserOptions: { parser: '@typescript-eslint/parser' },
    },
  },
)
