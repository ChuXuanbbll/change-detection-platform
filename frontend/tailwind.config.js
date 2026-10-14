/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    screens: {
      sm: '0px',
      md: '768px',
      lg: '1024px',
      xl: '1280px',
      '2xl': '1600px'
    },
    extend: {}
  },
  plugins: []
};
