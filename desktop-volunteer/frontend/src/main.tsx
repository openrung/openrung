import React from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { installThemeVariables } from './theme';
import './style.css';

installThemeVariables();

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
